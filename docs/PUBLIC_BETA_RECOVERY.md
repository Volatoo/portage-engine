# Public Beta recovery Gate

This runbook coordinates recovery evidence for the Public Beta readiness
decision. It does not claim that a local container test is a production drill,
and it does not overlap the release OCI build/promotion pipeline. The harness
reuses the Vault issuer integration Gate, pgBackRest restore path, immutable
object lifecycle binary, migration-binary schema contract (with the Public Beta
production upgrade history starting at v26), and isolated signer boundary.

## Status and evidence contract

Run the repository-only validation anywhere with Python 3, Bash, Git, and the
repository tools:

```bash
scripts/public-beta-recovery-drill.sh static
```

The command writes `summary.json`, `checks.jsonl`, and owner-only check logs
under `.local/recovery-evidence/<run-id>/`. `summary.json` uses schema version 1
and binds every check to the repository commit, owner, target, timestamps,
duration, command hash, and log hash. Static checks can pass without external
infrastructure, but top-level `live_gate_status` and the four live checks are
then recorded as `not-run`.
`not-run` is never promoted to `passed`.

Live evidence must use an explicit durable directory. Commands are retained
only as SHA-256 values; owner-only logs retain their output. Hook commands must
not print tokens, private keys, passphrases, database dumps, or unredacted
customer data.

All live phases require this contract before any operator hook runs:

```bash
export PORTAGE_DRILL_RUN_ID='20260801-public-beta-recovery-01'
export PORTAGE_DRILL_OWNER='alice@example.com'
export PORTAGE_DRILL_TARGET='shanghai-recovery-drill-01'
export PORTAGE_DRILL_ISOLATED=true
export PORTAGE_DRILL_CONFIRM=RUN_PUBLIC_BETA_RECOVERY_DRILL
export PORTAGE_DRILL_DESTRUCTIVE_CONFIRM=DESTROY_ISOLATED_DRILL_TARGET_ONLY
export PORTAGE_DRILL_EVIDENCE_DIR='/srv/audit/portage-engine/20260801-public-beta-recovery-01'
export PORTAGE_DRILL_ISOLATION_CHECK_CMD='/secure/drills/assert-isolated-target shanghai-recovery-drill-01'
```

The target must contain `drill`, `recovery`, `sandbox`, or `test`. A target
segment named `prod`, `production`, `live`, `main`, or `primary` is rejected.
The isolation command must independently prove account/project, network,
cluster, bucket, database, Vault mount, and signer restore boundaries relevant
to the selected phase. A no-op hook is rejected.

## Vault phase

Required operator facts:

- durable HA storage backend and topology;
- unseal mechanism plus at least two distinct unseal custodians;
- recovery-token mechanism plus at least two distinct recovery custodians;
- a non-production PKI mount and role;
- the exact bare common name (`portage-worker`) and allowed URI SAN
  (`spiffe://portage-engine/*` for the current attempt-bound identity
  contract); and
- access to a live `vault` CLI session without putting its token in a hook.

The harness directly parses `vault status -format=json` and the role returned
by `vault read -format=json`. It requires initialized, unsealed HA Vault with a
non-file storage type. The sign role must be client-only, limited to the exact
bare `portage-worker` common name and URI SAN, Ed25519 or EC P-256/P-384, and
no longer than 24 hours. Subdomains, glob domains and arbitrary SANs remain
disabled. The current Worker Gateway CSR uses Ed25519.

Configure the operator-specific ceremonies as commands that return zero only
after their real readbacks pass:

```bash
export VAULT_ADDR='https://vault-drill.example.net:8200'
export VAULT_CACERT='/secure/drills/vault-server-ca.pem'
export PORTAGE_VAULT_STORAGE_BACKEND='raft'
export PORTAGE_VAULT_UNSEAL_METHOD='cloud-kms plus recovery shares'
export PORTAGE_VAULT_UNSEAL_OWNERS='alice@example.com,bob@example.com'
export PORTAGE_VAULT_RECOVERY_OWNERS='carol@example.com,dave@example.com'
export PORTAGE_VAULT_DRILL_MOUNT='pki_workers_recovery_drill'
export PORTAGE_VAULT_DRILL_ROLE='portage-worker-recovery-drill'
export PORTAGE_VAULT_ALLOWED_URI_SAN='spiffe://portage-engine/*'
export PORTAGE_VAULT_ALLOWED_COMMON_NAME='portage-worker'

export PORTAGE_VAULT_UNSEAL_CHECK_CMD='/secure/drills/vault-ha-seal-unseal-and-readback'
export PORTAGE_VAULT_TOKEN_LOSS_CHECK_CMD='/secure/drills/vault-token-loss-must-fail'
export PORTAGE_VAULT_TOKEN_RECOVERY_CMD='/secure/drills/vault-reissue-minimal-token-and-sign'
export PORTAGE_VAULT_OLD_CA_CHECK_CMD='/secure/drills/vault-check-old-ca'
export PORTAGE_VAULT_DUAL_CA_CHECK_CMD='/secure/drills/vault-stage-dual-ca-and-check-both'
export PORTAGE_VAULT_NEW_CA_CHECK_CMD='/secure/drills/vault-drain-old-leaves-and-check-new-ca'
export PORTAGE_VAULT_LEAF_REVOKE_CMD='/secure/drills/vault-revoke-leaf-and-check-denial'
export PORTAGE_VAULT_ISSUER_REVOKE_CMD='/secure/drills/vault-revoke-old-issuer-and-check-denial'
export PORTAGE_VAULT_ISOLATED_RESTORE_CMD='/secure/drills/vault-restore-snapshot-into-isolated-ha-and-sign'

scripts/public-beta-recovery-drill.sh vault
```

The token-loss command must remove only the drill runtime's token, prove a sign
failure and degraded issuer health, and leave the old token unavailable. The
recovery command must issue a replacement with only
`update` on `<mount>/sign/<role>` and prove health recovery. The CA commands
must preserve the `old -> old+new -> new` trust sequence. Revocation checks
must exercise the application/CRL enforcement path, not merely inspect a Vault
write response. The isolated restore must use a new cluster identity and
network boundary.

`scripts/test-vault-issuer.sh` remains a disposable real-container integration
Gate for the application adapter. It is useful before the live phase but is not
HA or production recovery evidence.

## PostgreSQL phase

The phase requires a full backup, differential backup, continuous WAL check,
and time-targeted restore. `PGDATA` must be local block storage; NFS, SMB and
SSHFS are rejected. NFS is allowed only for the encrypted backup/WAL
repository and retained evidence.

After the full and differential backups, the checked-in preparation script
inserts a unique non-secret durable `audit_events.resource_id` marker, switches
WAL, selects a later recovery target, inserts a distinct post-target marker,
and switches WAL again. It writes the two timestamps to an owner-only state
file inside the evidence directory. The recovery target minus the durable
marker time is the measured RPO. A valid restore must contain the durable
marker and exclude the post-target marker. The harness measures RTO around the
restore command and records the backup repository size. The restore hook keeps
the ISO-8601 timestamp in evidence and converts it to pgBackRest's equivalent
space-separated, numeric-timezone form only for the restore invocation.

Example commands for the checked-in Compose pgBackRest topology:

```bash
export PORTAGE_MIGRATE_BIN='/opt/portage/bin/portage-migrate'
export PORTAGE_PGBACKREST_CIPHER_PASS='read-from-secret-provider'
export PORTAGE_PGBACKREST_REPO='/mnt/backup/portage-engine-recovery-drill-01'
export PORTAGE_PITR_ASSERT_RESOURCE_ID='public-beta-recovery-20260801-01'
export PORTAGE_PITR_ASSERT_ABSENT_RESOURCE_ID='public-beta-recovery-after-20260801-01'
export PORTAGE_PITR_STATE_FILE="$PORTAGE_DRILL_EVIDENCE_DIR/pitr-state.env"
export PORTAGE_PITR_MAX_RPO_SECONDS=60
export PORTAGE_PITR_MAX_RTO_SECONDS=1800

export PORTAGE_POSTGRES_PGDATA_FSTYPE_CMD='docker compose -f docker-compose.yml -f docker-compose.pgbackrest.yml exec -T postgres stat -f -c %T /var/lib/postgresql/18/docker'
export PORTAGE_POSTGRES_FULL_BACKUP_CMD='scripts/pgbackrest-backup.sh full'
export PORTAGE_POSTGRES_DIFF_BACKUP_CMD='scripts/pgbackrest-backup.sh diff'
export PORTAGE_POSTGRES_PITR_PREPARE_CMD='scripts/pgbackrest-pitr-prepare.sh'
export PORTAGE_POSTGRES_WAL_CHECK_CMD='docker compose -f docker-compose.yml -f docker-compose.pgbackrest.yml exec -T --user postgres postgres pgbackrest --stanza=portage-engine check'
export PORTAGE_POSTGRES_BACKUP_REPO="$PORTAGE_PGBACKREST_REPO"
export PORTAGE_POSTGRES_PITR_CMD='set -a; source "$PORTAGE_PITR_STATE_FILE"; set +a; scripts/pgbackrest-restore-drill.sh "$PORTAGE_PITR_TARGET_TIME"'

scripts/public-beta-recovery-drill.sh postgres
```

`PORTAGE_MIGRATE_BIN` must be the absolute path to the executable migration
binary deployed with the version under drill. Its database-free
`supported-schema` command reports the binary's supported range and latest
embedded migration as JSON; disagreement fails closed before restore. This
keeps the Gate aligned when reviewed migrations are added instead of treating
v26 as an eternal constant.

The preparation and restore hooks must use the same state file; do not edit its
timestamps. The restore script starts a temporary isolated PostgreSQL instance
and requires the restored database to equal that authoritative maximum. It
checks jobs, attempts, signing lineage, workload issuer/leaf lineage, capacity
pool/action/instance lineage, targets and the monitor view, project role
vocabulary, least-privilege app/signer/actuator database roles, and the PITR
two marker assertions. It records RPO, RTO, repository size, restored PGDATA
filesystem and owner as JSON. `scripts/postgres-restore-check.sh` is still available for a
logical dump check, but also requires the isolation confirmations and does not
replace the physical/WAL Gate.

## Object storage phase

Export the effective policy documents from the provider, not hand-written
intent documents. The API/read, executor, signer, quarantine-GC and
generation-GC principal IDs must all be different. The policy validator
rejects wildcard actions, API writes/deletes, quarantine deletion outside
`.quarantine/*`, and generation deletion outside `.generations/*`.

```bash
export PORTAGE_OBJECT_READ_POLICY='/srv/audit/input/api-read-policy.json'
export PORTAGE_OBJECT_EXECUTOR_POLICY='/srv/audit/input/executor-policy.json'
export PORTAGE_OBJECT_SIGNER_POLICY='/srv/audit/input/signer-policy.json'
export PORTAGE_OBJECT_QUARANTINE_GC_POLICY='/srv/audit/input/quarantine-gc-policy.json'
export PORTAGE_OBJECT_GENERATION_GC_POLICY='/srv/audit/input/generation-gc-policy.json'
export PORTAGE_OBJECT_READ_PRINCIPAL='arn:example:role/portage-api-read'
export PORTAGE_OBJECT_EXECUTOR_PRINCIPAL='arn:example:role/portage-executor'
export PORTAGE_OBJECT_SIGNER_PRINCIPAL='arn:example:role/portage-signer'
export PORTAGE_OBJECT_QUARANTINE_GC_PRINCIPAL='arn:example:role/portage-quarantine-gc'
export PORTAGE_OBJECT_GENERATION_GC_PRINCIPAL='arn:example:role/portage-generation-gc'
export PORTAGE_OBJECT_SOURCE_FAULT_DOMAIN='cn-east-1a/site-a'
export PORTAGE_OBJECT_REPLICA_FAULT_DOMAIN='cn-east-1c/site-b'

export PORTAGE_OBJECT_IMMUTABLE_CAS_CMD='/secure/drills/object-prove-create-only-and-stale-cas'
export PORTAGE_OBJECT_SOURCE_AUDIT_CMD='/secure/drills/run-as-auditor portage-artifact-lifecycle -operation audit -binhost-path releases/amd64/binpackages/23.0/target -arch amd64'
export PORTAGE_OBJECT_REPLICATION_CMD='/secure/drills/run-as-replicator portage-artifact-lifecycle -operation replicate -binhost-path releases/amd64/binpackages/23.0/target -arch amd64'
export PORTAGE_OBJECT_REPLICA_AUDIT_CMD='/secure/drills/run-as-replica-auditor portage-artifact-lifecycle -operation audit -binhost-path releases/amd64/binpackages/23.0/target -arch amd64'
export PORTAGE_OBJECT_RESTORE_CMD='/secure/drills/object-restore-stable-pointer-manifest-packages-artifacts'
export PORTAGE_OBJECT_ROLLBACK_CMD='/secure/drills/object-cas-rollback-to-previous-generation'
export PORTAGE_OBJECT_DEEP_AUDIT_CMD='/secure/drills/object-audit-active-and-rollback-generations'
export PORTAGE_OBJECT_QUARANTINE_GC_CMD='/secure/drills/run-as-quarantine-gc'
export PORTAGE_OBJECT_GENERATION_GC_CMD='/secure/drills/run-as-generation-gc portage-artifact-lifecycle -operation gc -retention 336h -binhost-path releases/amd64/binpackages/23.0/target -arch amd64'
export PORTAGE_OBJECT_REFERENCE_GC_CHECK_CMD='/secure/drills/object-prove-active-rollback-references-retained'

scripts/public-beta-recovery-drill.sh object
```

The restore command must recover and read back the channel pointer,
`manifest.json`, `Packages`, and every manifest-selected artifact from the
independently administered replica. The rollback must use the channel CAS,
then run a deep digest audit. GC evidence must show active, rollback and older
referenced generations retained. Provider versioning, retention/immutability,
credential revocation and cross-site timing remain provider-specific inputs.

## Signer phase

Perform backup on the offline signer trust domain. The backup command must
create an encrypted OpenPGP envelope; the harness rejects recognizable
plaintext private-key armor and parses the encrypted packet structure. Restore
both transition keys only into an isolated owner-only `GNUPGHOME` whose path
contains the drill target.

```bash
export PORTAGE_SIGNER_BACKUP_OFFLINE=true
export PORTAGE_SIGNER_CUSTODIANS='alice@example.com,bob@example.com'
export PORTAGE_SIGNER_SOURCE_GNUPGHOME='/var/lib/portage-signer/gpg'
export PORTAGE_SIGNER_RESTORE_GNUPGHOME='/srv/isolated/shanghai-recovery-drill-01/gnupg'
export PORTAGE_SIGNER_BACKUP_PATH='/srv/offline-backup/portage-signer-20260801.pgp'
export PORTAGE_SIGNER_OLD_KEY_ID='0123456789ABCDEF'
export PORTAGE_SIGNER_NEW_KEY_ID='FEDCBA9876543210'
export PORTAGE_SIGNER_BACKUP_RECIPIENT='recovery-custodians@example.com'

export PORTAGE_SIGNER_BACKUP_CMD='test ! -e "$PORTAGE_SIGNER_BACKUP_PATH" && GNUPGHOME="$PORTAGE_SIGNER_SOURCE_GNUPGHOME" gpg --batch --export-secret-keys "$PORTAGE_SIGNER_OLD_KEY_ID" "$PORTAGE_SIGNER_NEW_KEY_ID" | gpg --batch --yes --recipient "$PORTAGE_SIGNER_BACKUP_RECIPIENT" --encrypt --output "$PORTAGE_SIGNER_BACKUP_PATH"'
export PORTAGE_SIGNER_RESTORE_CMD='install -d -m 0700 "$PORTAGE_SIGNER_RESTORE_GNUPGHOME" && gpg --batch --decrypt "$PORTAGE_SIGNER_BACKUP_PATH" | gpg --batch --homedir "$PORTAGE_SIGNER_RESTORE_GNUPGHOME" --import'
export PORTAGE_SIGNER_OLD_VERIFY_CMD='/secure/drills/verify-gpkg-and-Packages-with-old-key'
export PORTAGE_SIGNER_NEW_VERIFY_CMD='/secure/drills/verify-gpkg-and-Packages-with-new-key'
export PORTAGE_SIGNER_OLD_GENERATION_CMD='/secure/drills/fetch-and-verify-old-published-generation'
export PORTAGE_SIGNER_ROLLBACK_CMD='/secure/drills/rollback-and-verify-with-dual-public-keyring'
export PORTAGE_SIGNER_BUILDER_NO_KEY_CMD='/secure/drills/assert-no-secret-key builder'
export PORTAGE_SIGNER_API_NO_KEY_CMD='/secure/drills/assert-no-secret-key api'
export PORTAGE_SIGNER_DASHBOARD_NO_KEY_CMD='/secure/drills/assert-no-secret-key dashboard'

scripts/public-beta-recovery-drill.sh signer
```

The two verification commands must cover GPKG payload signatures, the signed
GPKG Manifest, and the `Packages` signature. The old-generation and rollback
commands prove the dual public-key window does not strand retained content.
The absence commands must inspect the deployed filesystem, mounts, workload
secrets and `gpg --list-secret-keys` for each runtime; an image manifest alone
is not sufficient live evidence. Builder, API and Dashboard may receive the
public keyring only.

Run all phases only after every phase-specific input is present:

```bash
scripts/public-beta-recovery-drill.sh all
```

Separate phase runs are preferred because they retain clearer authority and
custodian boundaries.
