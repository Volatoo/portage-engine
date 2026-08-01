#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <owner/repository> <candidate|stable> <ghcr-release-manifest@sha256:...>" >&2
  exit 2
}

[ "$#" -eq 3 ] || usage
github_repository="$1"
channel="$2"
manifest_ref="$3"
case "$channel" in
  candidate|stable) ;;
  *) usage ;;
esac
if ! [[ "$github_repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  echo "invalid GitHub repository" >&2
  exit 2
fi
release_repository="ghcr.io/${github_repository,,}/releases"
if ! [[ "$manifest_ref" =~ ^${release_repository}@sha256:[0-9a-f]{64}$ ]]; then
  echo "manifest reference must be digest-only and belong to ${release_repository}" >&2
  exit 2
fi
for tool in cosign jq oras python3; do
  command -v "$tool" >/dev/null || {
    echo "required verifier is missing: $tool" >&2
    exit 2
  }
done

default_branch="${PORTAGE_RELEASE_DEFAULT_BRANCH:-main}"
issuer="https://token.actions.githubusercontent.com"
candidate_identity="^https://github.com/${github_repository}/\.github/workflows/release-candidate\.yml@refs/(heads/${default_branch}|tags/v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?)$"
stable_identity="^https://github.com/${github_repository}/\.github/workflows/release-(promote|rollback)\.yml@refs/heads/${default_branch}$"
if [ "$channel" = candidate ]; then
  manifest_identity="$candidate_identity"
else
  manifest_identity="$stable_identity"
fi

verify_tmp="$(mktemp -d)"
trap 'rm -rf -- "$verify_tmp"' EXIT
cosign verify --certificate-identity-regexp "$manifest_identity" \
  --certificate-oidc-issuer "$issuer" "$manifest_ref" >/dev/null
oras pull "$manifest_ref" -o "$verify_tmp"
cosign verify-blob --bundle "$verify_tmp/release-manifest.sigstore.json" \
  --certificate-identity-regexp "$manifest_identity" \
  --certificate-oidc-issuer "$issuer" \
  "$verify_tmp/release-manifest.json"
cosign verify-blob --bundle "$verify_tmp/SHA256SUMS.sigstore.json" \
  --certificate-identity-regexp "$candidate_identity" \
  --certificate-oidc-issuer "$issuer" \
  "$verify_tmp/SHA256SUMS"
manifest_digest="$(python3 scripts/release_manifest.py validate \
  --manifest "$verify_tmp/release-manifest.json" --root "$verify_tmp")"
if [ "$(jq -r '.release.channel' "$verify_tmp/release-manifest.json")" != "$channel" ]; then
  echo "signed manifest channel does not match the requested verification policy" >&2
  exit 1
fi
while IFS= read -r image; do
  cosign verify --certificate-identity-regexp "$candidate_identity" \
    --certificate-oidc-issuer "$issuer" "$image" >/dev/null
done < <(jq -r '.images[].reference' "$verify_tmp/release-manifest.json")

echo "verified ${channel} release manifest ${manifest_ref}"
echo "canonical manifest digest ${manifest_digest}"
jq -r '.images[] | "\(.role) \(.reference)"' "$verify_tmp/release-manifest.json"
