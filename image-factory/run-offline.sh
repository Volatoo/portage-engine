#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: STRICT_OFFLINE=1 $0 <base-systemd|desktop-verifier> <common.json> <offline-root> <inputs.lock.json>" >&2
}

if [[ $# -ne 4 ]]; then
  usage
  exit 2
fi
if [[ ${STRICT_OFFLINE:-} != 1 ]]; then
  echo "STRICT_OFFLINE=1 is required; enforce public-egress deny outside this script" >&2
  exit 2
fi

target=$1
common_vars=$2
offline_root=$3
input_lock=$4
case "$target" in
  base-systemd|desktop-verifier) ;;
  *) usage; exit 2 ;;
esac

factory_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$factory_dir/.." && pwd)

absolute_file() {
  local path=$1
  local directory
  directory=$(cd -- "$(dirname -- "$path")" && pwd)
  printf '%s/%s\n' "$directory" "$(basename -- "$path")"
}

absolute_dir() {
  cd -- "$1" && pwd
}

common_vars=$(absolute_file "$common_vars")
offline_root=$(absolute_dir "$offline_root")
input_lock=$(absolute_file "$input_lock")
packer_dir="$factory_dir/packer"
build_plan=${IMAGE_FACTORY_PLAN:-$factory_dir/plans/$target.build.json}
build_plan=$(absolute_file "$build_plan")
output_dir="$packer_dir/output"
report_path="$output_dir/$target.preflight.json"
plan_evidence_path="$output_dir/$target.plan-evidence.json"
source_evidence_path="$output_dir/$target.source-evidence.json"
generated_vars_path="$output_dir/$target.generated.pkrvars.json"
manifest_path="$output_dir/$target.image-manifest.json"
catalog_path="$output_dir/$target.catalog-candidate.json"
packer_manifest_path="$output_dir/$target.packer-manifest.json"
factory_bin=${IMAGE_FACTORY_BIN:-$repo_dir/bin/portage-image-factory}
factory_bin=$(absolute_file "$factory_bin")
packer_bin="$offline_root/packer/bin/packer"

for path in "$common_vars" "$input_lock" "$build_plan" "$factory_bin" "$packer_bin"; do
  if [[ ! -e $path ]]; then
    echo "required path is missing: $path" >&2
    exit 1
  fi
done
if [[ ! -x $factory_bin || ! -x $packer_bin ]]; then
  echo "image-factory and locked Packer binaries must be executable" >&2
  exit 1
fi
mkdir -p "$output_dir"
rm -f "$report_path" "$plan_evidence_path" "$source_evidence_path" "$generated_vars_path" "$manifest_path" "$catalog_path" "$packer_manifest_path"

"$factory_bin" preflight \
  -lock "$input_lock" -root "$offline_root" -target "$target" -report "$report_path"

"$factory_bin" plan \
  -common "$common_vars" -plan "$build_plan" -lock "$input_lock" -root "$offline_root" -target "$target" \
  -packer-manifest "$packer_manifest_path" -vars-output "$generated_vars_path" \
  -evidence-output "$plan_evidence_path"

export CHECKPOINT_DISABLE=1
export PACKER_NO_COLOR=1
export PACKER_PLUGIN_PATH="$offline_root/packer/plugins"

cd "$packer_dir"
"$packer_bin" plugins required .
"$packer_bin" validate -var-file="$generated_vars_path" .

"$factory_bin" source-check \
  -common "$common_vars" -plan "$build_plan" -lock "$input_lock" -root "$offline_root" \
  -packer-manifest "$packer_manifest_path" -evidence-output "$source_evidence_path"

"$packer_bin" build -var-file="$generated_vars_path" .

"$factory_bin" manifest \
  -common "$common_vars" -plan "$build_plan" -lock "$input_lock" -packer-manifest "$packer_manifest_path" \
  -output "$manifest_path" -catalog-output "$catalog_path"

echo "candidate manifest: $manifest_path"
echo "catalog candidate: $catalog_path"
