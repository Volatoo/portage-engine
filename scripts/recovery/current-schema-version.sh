#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"
mode="${1:-version}"
if [[ $# -gt 1 || ( "${mode}" != "version" && "${mode}" != "--json" ) ]]; then
  echo "usage: $0 [--json]" >&2
  exit 2
fi

schema_json=""
if [[ -n "${PORTAGE_MIGRATE_BIN:-}" ]]; then
  if [[ ! -x "${PORTAGE_MIGRATE_BIN}" ]]; then
    echo "PORTAGE_MIGRATE_BIN must name an executable migration binary" >&2
    exit 2
  fi
  schema_json="$("${PORTAGE_MIGRATE_BIN}" supported-schema)"
elif [[ -x "${repo_root}/bin/portage-migrate" ]]; then
  schema_json="$("${repo_root}/bin/portage-migrate" supported-schema)"
else
  schema_json="$(cd "${repo_root}" && go run ./cmd/migrate supported-schema)"
fi

python3 - "${schema_json}" "${mode}" <<'PY'
import json
import sys

try:
    support = json.loads(sys.argv[1])
except (TypeError, ValueError) as exc:
    raise SystemExit(f"migration binary returned invalid schema JSON: {exc}")

required = ("min", "max", "embedded_latest")
for name in required:
    value = support.get(name)
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise SystemExit(f"migration binary returned invalid {name}: {value!r}")
if support["min"] > support["max"]:
    raise SystemExit("migration binary returned an inverted schema range")
if support["max"] != support["embedded_latest"]:
    raise SystemExit(
        "migration binary maximum does not match its latest embedded migration"
    )

validated = {name: support[name] for name in required}
if sys.argv[2] == "--json":
    print(json.dumps(validated, sort_keys=True, separators=(",", ":")))
else:
    print(validated["max"])
PY
