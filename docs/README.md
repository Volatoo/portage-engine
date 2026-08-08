# Documentation

Start at the [project readme](../readme.md) for what Portage Engine is and how
to bring one up. This page is the map of everything else.

## Conventions

**Documentation is written in English**, in the same language as the code
comments, the commit messages and the CHANGELOG. A reader who can follow the
source can follow the docs, and there is one copy of every statement rather
than two that drift apart.

Every document below is one of four kinds, and the kind decides what belongs in
it:

| Kind | Answers | Changes when |
| --- | --- | --- |
| Reference | What is the contract? | The contract changes |
| Guide | How do I do this? | The procedure changes |
| Runbook | It is broken — what now? | The failure mode or the fix changes |
| Plan | What is not done yet? | Work lands, or the plan does |

A document that answers two of these is two documents. Reference docs describe
what the code does now and are wrong the moment it changes; plans describe what
it does not do yet and are wrong the moment it does.

## Reference

The contracts. These describe the system as it is.

- [Identity and project authorization](IAM.md) — authentication versus
  authorization, project RBAC, sessions, step-up, revocation.
- [Community identity providers](IDENTITY_PROVIDERS.md) — Authentik, Google,
  GitHub and generic OIDC, and why email is never a merge key.
- [Scheduler fairness and autoscaling](SCHEDULER.md) — project fairness,
  capacity-pool demand, lease fencing, and the Monitor read model.
- [Build catalog and profile registry](CATALOG.md) — the server-owned boundary
  between what a client may ask for and what infrastructure runs it.
- [Object storage contract](OBJECT_STORAGE.md) — S3 as an immutable bytes
  authority: quarantine, generations, ETag-fenced channel pointers.
- [Production boundary](PRODUCTION_BOUNDARY.md) — what `DEPLOYMENT_MODE=public`
  refuses that the trusted-LAN surface allows.
- [Compatibility policy](COMPATIBILITY.md) — what a pre-1.0 project promises
  across upgrades, and what it does not.
- [HTTP API](openapi.yaml) — the OpenAPI document for the `/api/v1` surface.
- [Design decisions](DESIGN_DECISIONS.md) — why the system is shaped this way
  and what was rejected; the reasoning the contracts above do not carry.

## Guides

How to do a thing.

- [Using Portage Engine](USAGE.md) — the two jobs it does, and how to keep them
  separate as a consumer and as an operator.
- [Using system Portage configuration](SYSTEM_CONFIG_USAGE.md) — handing the
  server the policy-validated subset of your own Portage config.
- [PVE Native Gentoo reference deployment](PVE_TESTING.md) — the tested
  Proxmox VE path end to end, and how to verify each stage.
- [Desktop E2E](DESKTOP_E2E.md) — verifying that a built package produces a
  desktop application that actually starts.
- [Distributed Build Alpha](DISTRIBUTED_BUILD_ALPHA.md) — the optional,
  default-off compile-only acceleration milestone.
- [Offline image factory](../image-factory/README.md) — building the base
  images in a namespace with no egress.

## Runbooks

For when something is wrong, or when you are proving it cannot go wrong.

- [Observability alert runbooks](OBSERVABILITY_RUNBOOKS.md) — one entry per
  shipped alert: what it means, what to look at, and how to drill it.
- [Public Beta recovery Gate](PUBLIC_BETA_RECOVERY.md) — the restore evidence
  required before Public Beta, and the inputs each drill needs.

## Plans

Not yet true. Read these for what is missing, never for how the system behaves.

- [Next steps and acceptance plan](NEXT_STEPS.md) — the remaining Public Beta
  work, split into what a repository gate can decide and what needs a real
  environment.

## Elsewhere in the repository

- [CHANGELOG](../CHANGELOG.md) — operator-visible changes, newest first.
- [SECURITY](../SECURITY.md) — reporting a vulnerability, and the release
  evidence a fix carries.
- [GOVERNANCE](../GOVERNANCE.md) and [MAINTAINERS](../MAINTAINERS.md).
- [Release process](../release/README.md) — candidate promotion, signing and
  the evidence each release must carry.
