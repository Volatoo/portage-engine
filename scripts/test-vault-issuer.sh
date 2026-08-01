#!/usr/bin/env bash
# Real Vault PKI integration and staged trust-bundle rollover Gate.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
readonly vault_image="${VAULT_TEST_IMAGE:-hashicorp/vault:2.0.3@sha256:a296a888b118615dc01d5f1a6846e6d4a7277946caaed5b447008fff5fe06b54}"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/portage-vault-test.XXXXXX")"
readonly test_root
readonly container_name="portage-vault-test-$$"
readonly tls_dir="${test_root}/tls"
readonly token_file="${test_root}/vault-token"
readonly server_ca="${test_root}/vault-server-ca.pem"
readonly old_ca="${test_root}/old-ca.pem"
readonly new_ca="${test_root}/new-ca.pem"
readonly rollover_bundle="${test_root}/rollover-ca.pem"

cleanup() {
    docker rm -f "${container_name}" >/dev/null 2>&1 || true
    rm -rf "${test_root}"
}
trap cleanup EXIT

mkdir -p "${tls_dir}"
chmod 0700 "${test_root}"
# The dev-only container runs as a non-root user and creates its generated TLS
# material here. The directory is deleted by the EXIT trap.
chmod 0777 "${tls_dir}"

docker run --detach --rm \
    --name "${container_name}" \
    --cap-add IPC_LOCK \
    --publish 127.0.0.1::8200 \
    --volume "${tls_dir}:/vault/tls" \
    "${vault_image}" \
    server -dev -dev-root-token-id=root -dev-tls \
    -dev-listen-address=0.0.0.0:8200 \
    -dev-tls-cert-dir=/vault/tls \
    -dev-tls-san=127.0.0.1 >/dev/null

vault_exec() {
    docker exec --interactive \
        --env VAULT_ADDR=https://127.0.0.1:8200 \
        --env VAULT_CACERT=/vault/tls/vault-ca.pem \
        --env VAULT_TOKEN=root \
        "${container_name}" vault "$@"
}

for _ in $(seq 1 60); do
    if vault_exec status >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
vault_exec status >/dev/null

published_port="$(docker port "${container_name}" 8200/tcp | sed -E 's/.*:([0-9]+)$/\1/')"
readonly published_port
readonly vault_address="https://127.0.0.1:${published_port}"

vault_exec secrets enable -path=pki_workers pki >/dev/null
vault_exec secrets tune -max-lease-ttl=8760h pki_workers >/dev/null
vault_exec write pki_workers/root/generate/internal \
    common_name=portage-worker-integration-root-1 ttl=8760h >/dev/null
vault_exec write pki_workers/roles/portage-worker \
    allow_any_name=false allow_bare_domains=true allow_glob_domains=false \
    allow_subdomains=false allowed_domains=portage-worker enforce_hostnames=false \
    allowed_uri_sans='spiffe://portage-engine/*' \
    use_csr_common_name=true use_csr_sans=true \
    server_flag=false client_flag=true code_signing_flag=false \
    email_protection_flag=false key_type=ed25519 \
    ttl=3h max_ttl=24h >/dev/null
vault_exec read -format=json pki_workers/roles/portage-worker \
    >"${test_root}/minimal-role.json"
python3 "${script_dir}/recovery/validate.py" vault-role \
    --file "${test_root}/minimal-role.json" \
    --allowed-common-name portage-worker \
    --allowed-uri-san 'spiffe://portage-engine/*' >/dev/null

printf '%s\n' \
    'path "pki_workers/sign/portage-worker" {' \
    '  capabilities = ["update"]' \
    '}' |
    vault_exec policy write portage-worker - >/dev/null

vault_exec token create -field=token \
    -policy=portage-worker -ttl=1h -renewable=true >"${token_file}"
chmod 0600 "${token_file}"
install -m 0600 "${tls_dir}/vault-ca.pem" "${server_ca}"
vault_exec read -field=certificate pki_workers/cert/ca >"${old_ca}"
chmod 0600 "${old_ca}"

run_gate() {
    PORTAGE_TEST_VAULT_ADDRESS="${vault_address}" \
    PORTAGE_TEST_VAULT_CA="$1" \
    PORTAGE_TEST_VAULT_SERVER_CA="${server_ca}" \
    PORTAGE_TEST_VAULT_TOKEN_FILE="${token_file}" \
    PORTAGE_TEST_EXPECT_TRUST_FAILURE="${2:-false}" \
    PORTAGE_TEST_TOKEN_RECOVERY="${3:-false}" \
        go test -tags=integration ./internal/workergateway \
        -run '^TestVaultIssuerIntegration$' -count=1
}

run_gate "${old_ca}" false true

new_issuer_id="$(
    vault_exec write -field=issuer_id \
        pki_workers/issuers/generate/root/internal \
        common_name=portage-worker-integration-root-2 ttl=8760h
)"
readonly new_issuer_id
vault_exec write pki_workers/config/issuers \
    default="${new_issuer_id}" default_follows_latest_issuer=false >/dev/null
vault_exec read -field=certificate \
    "pki_workers/issuer/${new_issuer_id}" >"${new_ca}"
chmod 0600 "${new_ca}"

# Switching Vault first must fail closed while a listener still trusts only
# the old generation.
run_gate "${old_ca}" true false

# The staged bundle accepts the new generation while retaining old leaves.
install -m 0600 "${old_ca}" "${rollover_bundle}"
printf '\n' >>"${rollover_bundle}"
sed -n '1,$p' "${new_ca}" >>"${rollover_bundle}"
run_gate "${rollover_bundle}" false false

# After the old leaf TTL/drain window, the old root can be removed.
run_gate "${new_ca}" false false

printf '%s\n' \
    "Vault issuer Gate passed: token recovery and old -> dual -> new CA rollover"
