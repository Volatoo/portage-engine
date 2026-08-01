#!/usr/bin/env bash
# Sourceable helper for release workflows.
#
# `oras resolve` reports "this tag does not exist" and "the registry could not
# answer" with the same non-zero status, so a write-once guard written as
# `if oras resolve ... >/dev/null 2>&1` reads a 5xx, a timeout, a rate limit or
# an expired token as proof that an already-approved immutable release tag is
# free to overwrite. Classify the failure instead.
#
#   source scripts/oras-resolve.sh
#   oras_resolve_into current_digest "ghcr.io/owner/name/releases:stable"
#
# Sets the named variable to the resolved sha256 digest, or to the literal
# "none" when the registry explicitly reports the reference as absent. Any other
# outcome returns 1, which under `set -e` fails the step rather than letting an
# ambiguous answer be read as "absent".
# The locals are prefixed because `printf -v` assigns through the caller's
# variable name, and a local of that same name would swallow the result.
oras_resolve_into() {
  local __oras_target="$1" __oras_reference="$2"
  local __oras_output __oras_digest __oras_lowered __oras_status=0
  __oras_output="$(oras resolve "${__oras_reference}" 2>&1)" || __oras_status=$?
  if [ "${__oras_status}" -eq 0 ]; then
    __oras_digest="$(printf '%s\n' "${__oras_output}" | tail -n 1)"
    if ! printf '%s' "${__oras_digest}" | grep -Eq '^sha256:[0-9a-f]{64}$'; then
      echo "oras resolve ${__oras_reference} returned an unusable digest: ${__oras_output}" >&2
      return 1
    fi
    printf -v "${__oras_target}" '%s' "${__oras_digest}"
    return 0
  fi
  __oras_lowered="$(printf '%s' "${__oras_output}" | tr '[:upper:]' '[:lower:]')"
  case "${__oras_lowered}" in
    *"not found"*|*name_unknown*|*manifest_unknown*|*"404 "*|*" 404"*)
      printf -v "${__oras_target}" '%s' none
      return 0
      ;;
  esac
  echo "oras resolve ${__oras_reference} failed: ${__oras_output}" >&2
  return 1
}
