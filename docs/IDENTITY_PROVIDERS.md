# Community identity providers

Portage Engine does not require Authentik. A deployment may enable any
combination of a generic OpenID Connect provider, Google OpenID Connect, and
GitHub OAuth 2.0. Authentik can itself use Google or GitHub as upstream
sources, but it is not a mandatory broker.

## Trust boundary

```text
browser
  ├─ OIDC Authorization Code + PKCE ──> Authentik / Google / generic OIDC
  └─ OAuth Authorization Code + PKCE ─> GitHub
                         │
                         ▼
                    Dashboard callback
                         │ upstream credential, one exchange only
                         ▼
                  Control-plane /iam/exchange
                    ├─ OIDC: signature + iss + aud + exp
                    └─ GitHub: OAuth App "check a token"
                         │
                         ▼
              random short-lived pe1_ session
              PostgreSQL stores SHA-256 only
```

The Dashboard never forwards an upstream token on ordinary API calls. The
control plane returns a random `pe1_` bearer session and stores only its
SHA-256 digest and bounded lifecycle metadata. A stolen GitHub token created
for another OAuth App is rejected because the exchange uses GitHub's
app-owner token check with this deployment's exact client ID and secret.

Authorization does not trust provider groups, repository membership or token
role claims. Projects and roles remain PostgreSQL-authoritative.

For a non-browser client that already obtained a provider credential, exchange
it without putting the credential in shell history:

```bash
read -r -s PORTAGE_PROVIDER_CREDENTIAL
export PORTAGE_PROVIDER_CREDENTIAL
PORTAGE_ENGINE_TOKEN="$(
  portage-client token-exchange -server=https://api.portage.example.org \
    -provider=google
)"
unset PORTAGE_PROVIDER_CREDENTIAL
export PORTAGE_ENGINE_TOKEN
```

This command is a credential exchange, not a complete OAuth device-login
implementation. A browser-assisted CLI/device flow remains a follow-up item.

## Provider registry

Copy `configs/auth-providers.example.json` to a deployment-owned file. Do not
put client secrets in JSON. Each `client_secret_env` names an environment
variable populated by Docker secrets, systemd credentials, Vault or another
secret provider.

Both `portage-server` and `portage-dashboard` must read the same registry:

```text
AUTH_PROVIDERS_PATH=/etc/portage-engine/configs/auth-providers.json
```

The repository Compose stack mounts `./configs` read-only at
`/etc/portage-engine/configs`. Provider IDs are stable configuration
identifiers and appear in callback paths.

## Callback URLs

Register one exact callback per provider:

```text
https://portage.example.org/auth/provider/authentik/callback
https://portage.example.org/auth/provider/google/callback
https://portage.example.org/auth/provider/github/callback
```

Trusted-LAN HTTP callbacks require the provider's explicit
`allow_insecure_http` opt-in. Internet-facing community deployments should
terminate HTTPS at a reverse proxy and set `COOKIE_SECURE=true`.

For OIDC, `audience` must be the client ID accepted by that issuer. Portage
Engine verifies discovery issuer, signature, audience, expiry and nonce.
Mutable email or username claims never identify an account.

For GitHub, create an OAuth App and put its client ID in the registry. Its
client-secret environment variable must be available to the Dashboard for code
exchange and to the control plane for the app-bound token check.

## Stable identity and administrators

The durable identity is `(issuer, subject)`, for example:

```text
(https://accounts.google.com, 10987654321)
(https://github.com, 88442211)
(https://auth.example.org/application/o/portage-engine/, 4f31...)
```

Bootstrap administrators are provider-scoped:

```text
AUTH_ADMIN_IDENTITIES=google:10987654321,github:88442211
```

Do not use email as an administrator key and do not automatically merge
accounts with matching email addresses. Direct Google login and Google
through Authentik remain two identities until a future user-initiated,
fresh-authenticated account-linking flow explicitly joins them.

Use `hybrid` during rollout so the independent legacy key remains a break-glass
path. Switch to `oidc` only after a provider-scoped administrator has logged in
and exercised a high-risk step-up action.

## Logout behavior

Local logout immediately revokes the current Portage Engine session. Revoke
all advances the subject watermark across every control-plane replica.

OIDC providers may additionally enable signed back-channel logout:

```json
{
  "backchannel_logout": true,
  "backchannel_require_sid": true,
  "backchannel_max_age_seconds": 300
}
```

Register the provider-specific endpoint:

```text
https://api.portage.example.org/api/v1/iam/providers/authentik/backchannel-logout
```

Startup fails closed if logout is enabled but discovery does not advertise the
required capability. Logout JWTs are checked for signature, issuer, audience,
expiry, event, absence of nonce, `jti`, and `sub`/`sid`. PostgreSQL stores only
an issuer-bound SHA-256 of `jti`, making retries idempotent without retaining
the raw token.

GitHub OAuth has no OIDC back-channel logout. Google should only enable this
option if its discovery metadata advertises it. Platform idle/max lifetime and
user/admin revocation remain mandatory for every provider.

GitHub's OAuth token check also does not assert a recent MFA time. Direct
GitHub login is therefore valid for ordinary project access but fails closed
for operations that require `auth_time` step-up. Use an OIDC provider that
emits verified `auth_time`/AMR/ACR for current administrator step-up. Native
Portage Engine WebAuthn/TOTP step-up is a separate follow-up milestone; the
service does not manufacture a fresh-auth claim from an OAuth callback.

## Operational checks

Before removing the legacy break-glass path:

1. Confirm each configured button starts only its own provider flow.
2. Confirm a GitHub token from a different OAuth App cannot be exchanged.
3. Confirm `/api/v1/iam/me` returns the expected issuer and subject.
4. Confirm a revoked `pe1_` token fails on both control-plane replicas.
5. Replay one signed logout token and confirm it is a successful no-op.
6. Confirm logs contain no upstream credential, `pe1_` bearer, raw logout JWT
   or raw `jti`.
