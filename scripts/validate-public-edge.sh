#!/usr/bin/env bash
set -u -o pipefail

baseline_commit="f79fca4"
output_path=""

usage() {
  echo "usage: $0 [--output PATH]" >&2
}

while (( $# > 0 )); do
  case "$1" in
    --output)
      if (( $# < 2 )); then
        usage
        exit 2
      fi
      output_path="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/portage-public-edge.XXXXXX")"
checks_file="${temp_dir}/checks.jsonl"
compose_json="${temp_dir}/compose.json"
rendered_nginx="${temp_dir}/default.conf"
failure_count=0
not_run_count=0
validated_edge_image="${PORTAGE_EDGE_IMAGE:-nginx:1.29-alpine}"
validated_edge_digest=""

cleanup() {
  rm -rf "${temp_dir}"
}
trap cleanup EXIT

record_check() {
  local check_id="$1"
  local status="$2"
  local summary="$3"
  local reason="${4:-}"
  jq -cn \
    --arg id "${check_id}" \
    --arg status "${status}" \
    --arg summary "${summary}" \
    --arg reason "${reason}" \
    '{id: $id, status: $status, summary: $summary} +
     (if $reason == "" then {} else {reason: $reason} end)' \
    >>"${checks_file}"
  case "${status}" in
    fail) failure_count=$((failure_count + 1)) ;;
    not_run) not_run_count=$((not_run_count + 1)) ;;
  esac
}

run_check() {
  local check_id="$1"
  local summary="$2"
  shift 2
  if "$@"; then
    record_check "${check_id}" pass "${summary}"
  else
    record_check "${check_id}" fail "${summary}" "repository contract mismatch"
  fi
}

check_required_files() {
  local relative
  for relative in \
    docker-compose.public.yml \
    configs/auth-providers.example.json \
    deploy/public-edge/docker-compose.edge.yml \
    deploy/public-edge/nginx.conf.template \
    docs/PRODUCTION_BOUNDARY.md; do
    [[ -f "${repo_root}/${relative}" ]] || return 1
  done
}

check_provider_registry() {
  jq -e '
    (.providers | length) == 3 and
    ([.providers[].id] | sort) == ["authentik", "github", "google"] and
    all(.providers[];
      . as $provider |
      ($provider.type == "oidc" or $provider.type == "github") and
      ($provider.redirect_url | startswith("https://")) and
      ($provider.redirect_url | contains("/auth/provider/" + $provider.id + "/callback")) and
      ($provider.client_secret_env | type == "string" and length > 0) and
      ($provider | has("client_secret") | not)
    )
  ' "${repo_root}/configs/auth-providers.example.json" >/dev/null
}

check_public_compose_contract() {
  jq -e '
    def envmap:
      map(capture("^(?<key>[^=]+)=(?<value>.*)$")) |
      map({(.key): .value}) | add;
    (.services["portage-server"].environment | envmap) as $server |
    (.services["portage-dashboard"].environment | envmap) as $dashboard |
    $server.DEPLOYMENT_MODE == "public" and
    $server.AUTH_MODE == "oidc" and
    $server.API_KEY == "" and
    $server.STEP_UP_API_KEY == "" and
    ($server.CORS_ALLOWED_ORIGINS | contains("PORTAGE_CORS_ALLOWED_ORIGINS")) and
    ($server.OIDC_SESSION_IDLE_MINUTES | contains("PORTAGE_OIDC_SESSION_IDLE_MINUTES")) and
    ($server.OIDC_SESSION_MAX_MINUTES | contains("PORTAGE_OIDC_SESSION_MAX_MINUTES")) and
    ($server.OIDC_STEP_UP_MAX_AGE_MINUTES | contains("PORTAGE_OIDC_STEP_UP_MAX_AGE_MINUTES")) and
    ($server.METRICS_PASSWORD | contains("PORTAGE_METRICS_PASSWORD")) and
    ($server.TRUSTED_PROXY_CIDRS | contains("PORTAGE_TRUSTED_PROXY_CIDRS")) and
    $dashboard.DEPLOYMENT_MODE == "public" and
    $dashboard.AUTH_ENABLED == "true" and
    $dashboard.ALLOW_ANONYMOUS == "false" and
    $dashboard.COOKIE_SECURE == "true" and
    ($dashboard.JWT_SECRET | contains("PORTAGE_DASHBOARD_JWT_SECRET")) and
    $dashboard.AUTH_PROVIDERS_PATH == "/etc/portage-engine/configs/auth-providers.json" and
    $dashboard.ADMIN_USER == "" and
    $dashboard.ADMIN_PASSWORD == "" and
    $dashboard.SERVER_API_KEY == "" and
    $dashboard.SERVER_STEP_UP_API_KEY == ""
  ' "${compose_json}" >/dev/null
}

check_backend_ports() {
  jq -e '
    ((.services["portage-server"].ports // []) | length) == 1 and
    (.services["portage-server"].ports[0] | endswith(":9443")) and
    ((.services["portage-dashboard"].ports // []) | length) == 0 and
    ((.services["portage-edge"].ports // []) | length) == 2 and
    (.services["portage-edge"].ports | any(endswith(":80:8080"))) and
    (.services["portage-edge"].ports | any(endswith(":443:8443"))) and
    ([.services | to_entries[] |
      select(((.value.ports // []) | length) > 0) | .key] | sort) ==
      ["portage-edge", "portage-server"] and
    (.networks["public-edge-backend"].internal == true)
  ' "${compose_json}" >/dev/null
}

check_edge_static_contract() {
  local config="${repo_root}/deploy/public-edge/nginx.conf.template"
  local device_identity_block device_token_block log_format_block
  device_identity_block="$(awk '
    $0 == "    location ^~ /api/v1/iam/device/ {" { emit = 1 }
    emit { print }
    emit && $0 == "    }" { exit }
  ' "${config}")"
  device_token_block="$(awk '
    $0 == "    location = /api/v1/iam/device/token {" { emit = 1 }
    emit { print }
    emit && $0 == "    }" { exit }
  ' "${config}")"
  # The access-log assertion has to see the whole directive: the real
  # log_format spans three lines and ripgrep matches one line at a time.
  log_format_block="$(awk '
    /^log_format / { emit = 1 }
    emit { print }
    emit && /;[[:space:]]*$/ { exit }
  ' "${config}")"
  rg -q 'limit_conn_zone \$binary_remote_addr zone=source_ip_connections:' "${config}" &&
    rg -q 'limit_req_zone \$binary_remote_addr zone=public_requests:' "${config}" &&
    rg -q 'limit_req_zone \$binary_remote_addr zone=identity_requests:' "${config}" &&
    rg -q 'limit_req_zone \$binary_remote_addr zone=device_token_requests:10m rate=30r/m;' "${config}" &&
    rg -q '^limit_req_status 429;' "${config}" &&
    rg -q '^limit_conn_status 429;' "${config}" &&
    [[ -n "${device_token_block}" ]] &&
    rg -q 'limit_req zone=device_token_requests burst=10 nodelay;' <<<"${device_token_block}" &&
    ! rg -q 'zone=identity_requests' <<<"${device_token_block}" &&
    [[ -n "${device_identity_block}" ]] &&
    rg -q 'limit_req zone=public_requests burst=10 nodelay;' <<<"${device_identity_block}" &&
    rg -q 'limit_req zone=identity_requests burst=5 nodelay;' <<<"${device_identity_block}" &&
    rg -q 'add_header Cache-Control "no-store" always;' <<<"${device_identity_block}" &&
    rg -q 'proxy_cookie_flags ~ secure httponly samesite=lax;' "${config}" &&
    rg -q 'location = /api/v1/instances/shell \{ return 404; \}' "${config}" &&
    rg -q 'location \^~ /shell/ \{ return 404; \}' "${config}" &&
    rg -q 'auth_basic_user_file /run/secrets/portage_metrics_htpasswd;' "${config}" &&
    rg -q 'location = /metrics/prometheus' "${config}" &&
    rg -q 'location ~ \^/\(packages\|docs\|status\)\$' "${config}" &&
    rg -q 'location \^~ /binpkgs/' "${config}" &&
    rg -q 'proxy_set_header X-Forwarded-For \$remote_addr;' "${config}" &&
    ! rg -q '\$proxy_add_x_forwarded_for' "${config}" &&
    rg -q '\$remote_addr - \$request_method \$uri \$status' "${config}" &&
    [[ -n "${log_format_block}" ]] &&
    ! rg -qF '$request_uri' <<<"${log_format_block}"
}

check_documented_boundary() {
  local document="${repo_root}/docs/PRODUCTION_BOUNDARY.md"
  rg -q 'Public Beta.*WebShell|Public Beta.*web shell|does not route.*WebShell' "${document}" &&
    rg -q 'source-IP|source IP' "${document}" &&
    rg -q 'not.run|not run|未执行' "${document}" &&
    rg -q 'evidence' "${document}"
}

render_nginx() {
  sed \
    -e 's/${PORTAGE_PUBLIC_HOST}/portage.example.test/g' \
    -e 's/${PORTAGE_API_HOST}/api.portage.example.test/g' \
    -e 's/${PORTAGE_METRICS_HOST}/metrics.portage.example.test/g' \
    "${repo_root}/deploy/public-edge/nginx.conf.template" >"${rendered_nginx}"
}

validate_nginx_in_container() {
  local cert_path="${temp_dir}/validation.crt"
  local key_path="${temp_dir}/validation.key"
  local htpasswd_path="${temp_dir}/metrics.htpasswd"
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
    -subj '/CN=portage.example.test' \
    -keyout "${key_path}" -out "${cert_path}" >/dev/null 2>&1 || return 1
  printf 'metrics:$2y$05$static.validation.only.not.a.credential\n' >"${htpasswd_path}"
  render_nginx || return 1
  docker run --rm \
    --entrypoint nginx \
    --add-host portage-dashboard:127.0.0.1 \
    --add-host portage-server:127.0.0.1 \
    -v "${rendered_nginx}:/etc/nginx/conf.d/default.conf:ro" \
    -v "${cert_path}:/run/secrets/portage_public_tls_cert:ro" \
    -v "${key_path}:/run/secrets/portage_public_tls_key:ro" \
    -v "${htpasswd_path}:/run/secrets/portage_metrics_htpasswd:ro" \
    "${validated_edge_image}" -t >/dev/null || return 1
  validated_edge_digest="$(
    docker image inspect --format '{{join .RepoDigests ","}}' \
      "${validated_edge_image}" 2>/dev/null || true
  )"
}

cd "${repo_root}" || exit 2

if ! command -v jq >/dev/null 2>&1 || ! command -v rg >/dev/null 2>&1; then
  echo "jq and rg are required" >&2
  exit 2
fi

run_check repository_inputs "required public deployment inputs are present" check_required_files
run_check provider_registry "provider registry has Authentik, Google, and GitHub HTTPS callbacks without embedded secrets" check_provider_registry
run_check edge_static_policy "edge template contains independent source-IP/request limits, device IAM no-store policy, cookie policy, anonymous routes, metrics auth, and WebShell denial" check_edge_static_contract
run_check production_boundary_documented "production boundary documents evidence and not-run semantics" check_documented_boundary

if ! command -v docker >/dev/null 2>&1; then
  record_check compose_render not_run "Compose overlay renders without interpolating production credentials" "docker is unavailable"
  record_check backend_isolation not_run "only edge HTTP(S) and private worker mTLS publish host ports" "Compose render was not run"
  record_check public_runtime_policy not_run "public server and Dashboard use fail-closed identity/session settings" "Compose render was not run"
  record_check nginx_syntax not_run "rendered nginx reference passes nginx -t" "docker is unavailable"
elif docker compose \
  -f docker-compose.yml \
  -f docker-compose.public.yml \
  -f deploy/public-edge/docker-compose.edge.yml \
  config --no-interpolate --format json >"${compose_json}"; then
  record_check compose_render pass "Compose overlay renders without interpolating production credentials"
  run_check backend_isolation "only edge HTTP(S) and private worker mTLS publish host ports" check_backend_ports
  run_check public_runtime_policy "public server and Dashboard use fail-closed identity/session settings" check_public_compose_contract
  if validate_nginx_in_container; then
    record_check nginx_syntax pass "rendered nginx reference passes nginx -t"
  else
    record_check nginx_syntax fail "rendered nginx reference passes nginx -t" "container validation failed"
  fi
else
  record_check compose_render fail "Compose overlay renders without interpolating production credentials" "docker compose config failed"
  record_check backend_isolation not_run "only edge HTTP(S) and private worker mTLS publish host ports" "Compose render failed"
  record_check public_runtime_policy not_run "public server and Dashboard use fail-closed identity/session settings" "Compose render failed"
  record_check nginx_syntax not_run "rendered nginx reference passes nginx -t" "Compose render failed"
fi

overall_status="pass"
exit_code=0
if (( failure_count > 0 )); then
  overall_status="fail"
  exit_code=1
elif (( not_run_count > 0 )); then
  overall_status="not_run"
  exit_code=3
fi

repository_head="$(git rev-parse HEAD 2>/dev/null || printf unknown)"
working_tree_dirty=false
if [[ -n "$(git status --short 2>/dev/null)" ]]; then
  working_tree_dirty=true
fi
generated_at="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
manifest="$(jq -s \
  --arg generated_at "${generated_at}" \
  --arg baseline_commit "${baseline_commit}" \
  --arg repository_head "${repository_head}" \
  --arg validation_image "${validated_edge_image}" \
  --arg validation_image_digest "${validated_edge_digest}" \
  --argjson working_tree_dirty "${working_tree_dirty}" \
  --arg status "${overall_status}" \
  '{schema_version: 1, gate: "public_edge_repository", generated_at: $generated_at,
    baseline_commit: $baseline_commit, repository_head: $repository_head,
    tree_basis: "repository_head_plus_worktree",
    working_tree_dirty: $working_tree_dirty,
    validation_image: $validation_image,
    validation_image_digest: $validation_image_digest,
    status: $status, credentials_used: false, secrets_persisted: false,
    checks: .}' \
  "${checks_file}")"

if [[ -n "${output_path}" ]]; then
  output_dir="$(dirname "${output_path}")"
  mkdir -p "${output_dir}"
  umask 077
  printf '%s\n' "${manifest}" >"${output_path}"
else
  printf '%s\n' "${manifest}"
fi

exit "${exit_code}"
