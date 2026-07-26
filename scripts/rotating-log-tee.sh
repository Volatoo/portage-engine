#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 1 ]]; then
  echo "usage: $0 LOG_FILE" >&2
  exit 2
fi

log_file="$1"
max_bytes="${PORTAGE_LOG_MAX_BYTES:-10485760}"
backup_file="${log_file}.1"

if [[ ! "${max_bytes}" =~ ^[1-9][0-9]*$ ]]; then
  echo "PORTAGE_LOG_MAX_BYTES must be a positive integer" >&2
  exit 2
fi

mkdir -p "$(dirname "${log_file}")"
touch "${log_file}"
current_bytes="$(wc -c <"${log_file}")"
line=""

while IFS= read -r line || [[ -n "${line}" ]]; do
  printf '%s\n' "${line}"
  printf '%s\n' "${line}" >>"${log_file}"
  current_bytes=$((current_bytes + ${#line} + 1))

  if (( current_bytes >= max_bytes )); then
    mv -f "${log_file}" "${backup_file}"
    : >"${log_file}"
    current_bytes=0
  fi
done
