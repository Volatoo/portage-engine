# IMG-4 operations control plane

This directory contains reviewable examples for bundle signing, periodic
rebuild decisions, promotion, rollback, and lease-aware cleanup. The commands
are deliberately split across two trust zones:

- the connected sync zone owns the bundle private key and runs `bundle-seal`;
- the release controller owns a different release private key and runs
  `promote` or `rollback` only after all evidence is present;
- builders and PVE nodes receive public keys only.

Generate both key pairs once and store the private files outside the repository:

```bash
portage-image-factory ops-keygen \
  -private /secure/sync-private.json \
  -public /srv/offline/trust/sync-public.json

portage-image-factory ops-keygen \
  -private /secure/release-private.json \
  -public /srv/releases/trust/release-public.json
```

After synchronization has produced a complete `inputs.lock.json`, seal it.
The lock must contain a non-future advisory cutoff and no placeholder digest:

```bash
portage-image-factory bundle-seal \
  -lock /srv/offline/inputs.lock.json \
  -root /srv/offline \
  -private-key /secure/sync-private.json \
  -fresh-hours 336 \
  -manifest-output /srv/offline/bundle-manifest.json \
  -signature-output /srv/offline/bundle-manifest.sig.json
```

Transfer only the bundle, manifest, signature, and sync public key into the
factory zone. `bundle-verify` re-hashes every locked object and fails when the
freshness window has elapsed.

Promotion is release-group atomic: every profile/image in the candidate
catalog must have a matching image manifest, successful Terraform smoke with
destroy evidence, and a PVE output stamp. A desktop image additionally requires
a passing deterministic Desktop E2E result with restore/start, accessibility,
screenshot and stop evidence; its digest is included in the signed promotion
receipt. A base image is rejected if it carries an unrelated desktop result.
Do not hand-merge the per-image
fragments. Put `candidate-catalog.example.json` at the root of a confined
evidence tree and run:

```bash
portage-image-factory catalog-assemble \
  -spec /srv/promotion/candidate-catalog.json \
  -output /srv/promotion/candidate-catalog.generated.json
```

The assembler re-verifies each image's BuildPlan, common config, input lock,
signed bundle, repository snapshot digest, and freshness window. Base and
desktop may therefore use different immutable MirrorBundle IDs; promotion
verifies every member of the `bundles` array and binds the image's
`input_lock_digest` to the matching signed bundle. This avoids extending or
silently replacing the already-smoke-tested base bundle when desktop inputs
are added later.

Obtain the generated catalog digest with `ops-digest`, fill
`promotion-plan.example.json`, then run `promote`. Paths in `bundles` and
`evidence` are relative to `-evidence-root` and may not escape it. The plan
cannot nominate its own trust key: every bundle is checked with the
release-controller-owned `-bundle-public-key`. Legacy single-bundle plans
remain supported through the other command-line bundle flags.
The command writes an immutable release directory first and atomically
replaces one signed alias envelope last. `expected_previous_release_id` is the
compare-and-swap guard.

Rollback accepts only a previously signed promotion receipt and stable catalog.
It requires every bundle recorded by that receipt to remain fresh and to match
the stable catalog. It writes another signed alias revision and does not mutate
or overwrite an immutable release directory.

`rebuild-plan` and `cleanup-plan` are intentionally non-destructive. Before
cleanup planning, the lease authority runs `state-sign`; `cleanup-plan` rejects
an unsigned, modified, expired, future-dated, or longer-than-one-hour
generation/lease snapshot. This short validity window prevents a previously
valid snapshot from being replayed after a new lease is created. Schedule
the former from systemd/cron and dispatch builds for entries with `due: true`.
The latter emits `delete` only for a retired generation that is outside the
retention window, not selected by an alias, and has no unexpired active lease.
PVE/template deletion remains a separately reviewed apply step.
