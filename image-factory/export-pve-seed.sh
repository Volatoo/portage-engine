#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: sudo NAS_API_URL=https://mirror.internal NAS_USERNAME=... NAS_PASSWORD=... \
  NAS_DIRECTORY=portage-engine/image-factory/seeds \
  export-pve-seed.sh <template-vmid> <backup-volid> <manifest-output.json>

The command must run on the PVE node that owns the directory-storage backup.
NAS_CA_BUNDLE may name an internal CA bundle. Plain HTTP is intentionally rejected.
EOF
}

if [[ $# -ne 3 ]]; then
  usage
  exit 2
fi
if [[ $(id -u) -ne 0 ]]; then
  echo "seed export must run as root on the PVE node" >&2
  exit 1
fi

vmid=$1
backup_volid=$2
manifest_output=$3
for name in NAS_API_URL NAS_USERNAME NAS_PASSWORD NAS_DIRECTORY; do
  if [[ -z ${!name:-} ]]; then
    echo "missing required environment value: $name" >&2
    exit 2
  fi
done
if [[ ! $vmid =~ ^[1-9][0-9]{2,8}$ ]]; then
  echo "invalid template VMID" >&2
  exit 2
fi
if [[ ! $backup_volid =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*:backup/vzdump-qemu-${vmid}-[a-zA-Z0-9_.-]+\.vma\.zst$ ]]; then
  echo "backup volume is not a zstd VMA archive for VMID $vmid" >&2
  exit 2
fi
if [[ ! $NAS_API_URL =~ ^https://[a-zA-Z0-9._:-]+(/[-a-zA-Z0-9._~:/%+]*)?$ ]]; then
  echo "NAS_API_URL must use HTTPS; seed export over plaintext HTTP is forbidden" >&2
  exit 2
fi
if [[ ! $NAS_DIRECTORY =~ ^[a-zA-Z0-9][a-zA-Z0-9._/-]*$ || $NAS_DIRECTORY == *..* || $NAS_DIRECTORY == */ ]]; then
  echo "NAS_DIRECTORY is invalid" >&2
  exit 2
fi
if [[ $manifest_output != *.json || -e $manifest_output ]]; then
  echo "manifest output must be a new .json file" >&2
  exit 2
fi

for command in curl jq pvesm qm sha256sum stat; do
  command -v "$command" >/dev/null || {
    echo "required command is missing: $command" >&2
    exit 1
  }
done

status=$(qm status "$vmid" | awk '{print $2}')
config=$(qm config "$vmid")
if [[ $status != stopped ]] || ! grep -qx 'template: 1' <<<"$config"; then
  echo "VMID $vmid must be a stopped QEMU template" >&2
  exit 1
fi
template_name=$(sed -n 's/^name: //p' <<<"$config")
if [[ -z $template_name || ! $template_name =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]]; then
  echo "template has an invalid or missing name" >&2
  exit 1
fi

backup_path=$(pvesm path "$backup_volid")
if [[ ! -f $backup_path || -L $backup_path ]]; then
  echo "backup volume does not resolve to a regular non-symlink file" >&2
  exit 1
fi
archive_size=$(stat -c '%s' -- "$backup_path")
archive_sha256=$(sha256sum -- "$backup_path" | awk '{print $1}')
if [[ ! $archive_sha256 =~ ^[a-f0-9]{64}$ || $archive_size -le 0 ]]; then
  echo "failed to calculate seed archive identity" >&2
  exit 1
fi

cookie_jar=$(mktemp)
login_body=$(mktemp)
upload_body=$(mktemp)
remote_copy=$(mktemp)
chmod 0600 "$cookie_jar" "$login_body" "$upload_body" "$remote_copy"
cleanup() {
  rm -f -- "$cookie_jar" "$login_body" "$upload_body" "$remote_copy"
}
trap cleanup EXIT

curl_tls=()
if [[ -n ${NAS_CA_BUNDLE:-} ]]; then
  curl_tls=(--cacert "$NAS_CA_BUNDLE")
fi
api_base=${NAS_API_URL%/}
export NAS_USERNAME NAS_PASSWORD
jq -n '{username: env.NAS_USERNAME, password: env.NAS_PASSWORD}' >"$login_body"
unset NAS_PASSWORD
curl --fail --silent --show-error "${curl_tls[@]}" \
  --cookie-jar "$cookie_jar" --header 'Content-Type: application/json' \
  --data-binary "@$login_body" "$api_base/api/auth/login" >/dev/null

artifact_name="sha256-${archive_sha256}.vma.zst"
curl --fail --silent --show-error "${curl_tls[@]}" \
  --cookie "$cookie_jar" \
  --form "file=@${backup_path};filename=${artifact_name}" \
  --form "directory=${NAS_DIRECTORY}" --form 'overwrite=true' \
  "$api_base/api/artifacts" >"$upload_body"

artifact_url=$(jq -r '.artifact.url // empty' "$upload_body")
if [[ -z $artifact_url ]]; then
  artifact_url="$api_base/local/$NAS_DIRECTORY/$artifact_name"
elif [[ $artifact_url == /* ]]; then
  artifact_url="$api_base$artifact_url"
fi
if [[ ! $artifact_url =~ ^https://[a-zA-Z0-9._:-]+/[-a-zA-Z0-9._~:/%+]+$ ]]; then
  echo "artifact API returned an unsafe seed URL" >&2
  exit 1
fi

curl --fail --silent --show-error "${curl_tls[@]}" "$artifact_url" >"$remote_copy"
remote_size=$(stat -c '%s' -- "$remote_copy")
remote_sha256=$(sha256sum -- "$remote_copy" | awk '{print $1}')
if [[ $remote_size -ne $archive_size || $remote_sha256 != "$archive_sha256" ]]; then
  echo "uploaded seed failed independent read-back verification" >&2
  exit 1
fi

exported_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
jq -n \
  --arg exported_at "$exported_at" \
  --arg node "$(hostname -s)" \
  --arg template_name "$template_name" \
  --arg backup_volid "$backup_volid" \
  --arg artifact_url "$artifact_url" \
  --arg digest "sha256:$archive_sha256" \
  --argjson vmid "$vmid" \
  --argjson size "$archive_size" \
  '{schema_version:1, exported_at:$exported_at, pve_node:$node,
    source_template:{vmid:$vmid,name:$template_name,state:"stopped",template:true},
    backup_volid:$backup_volid,
    artifact:{url:$artifact_url,digest:$digest,size:$size,format:"vma.zst"},
    read_back_verified:true}' >"$manifest_output"
chmod 0644 "$manifest_output"

manifest_name="sha256-${archive_sha256}.seed-manifest.json"
curl --fail --silent --show-error "${curl_tls[@]}" \
  --cookie "$cookie_jar" \
  --form "file=@${manifest_output};filename=${manifest_name}" \
  --form "directory=${NAS_DIRECTORY}" --form 'overwrite=true' \
  "$api_base/api/artifacts" >"$upload_body"
manifest_url=$(jq -r '.artifact.url // empty' "$upload_body")
if [[ -z $manifest_url ]]; then
  manifest_url="$api_base/local/$NAS_DIRECTORY/$manifest_name"
elif [[ $manifest_url == /* ]]; then
  manifest_url="$api_base$manifest_url"
fi
if [[ ! $manifest_url =~ ^https://[a-zA-Z0-9._:-]+/[-a-zA-Z0-9._~:/%+]+$ ]]; then
  echo "artifact API returned an unsafe seed manifest URL" >&2
  exit 1
fi
curl --fail --silent --show-error "${curl_tls[@]}" "$manifest_url" >"$remote_copy"
if ! cmp -s -- "$manifest_output" "$remote_copy"; then
  echo "uploaded seed manifest failed independent read-back verification" >&2
  exit 1
fi

marker="portage-engine-provenance=sha256:${archive_sha256}"
existing_marker=$(grep -o 'portage-engine-provenance=sha256:[a-f0-9]\{64\}' <<<"$config" || true)
if [[ -n $existing_marker && $existing_marker != "$marker" ]]; then
  echo "template already carries a different provenance marker" >&2
  exit 1
fi
if [[ -z $existing_marker ]]; then
  description=$(sed -n 's/^description: //p' <<<"$config")
  if [[ -n $description ]]; then
    description="$description | $marker"
  else
    description="$marker"
  fi
  qm set "$vmid" --description "$description" >/dev/null
fi

echo "seed exported and read-back verified: sha256:$archive_sha256"
echo "template provenance stamped: VMID $vmid ($template_name)"
echo "manifest: $manifest_output"
