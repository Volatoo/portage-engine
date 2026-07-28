#!/usr/bin/env bash
set -euo pipefail

target_dir="${1:-.local/worker-pki}"
gateway_host="${WORKER_GATEWAY_HOST:-}"

if [[ -z "${gateway_host}" ]]; then
  echo "WORKER_GATEWAY_HOST must be the LAN DNS name or IP used by builders" >&2
  exit 2
fi
if [[ "${gateway_host}" == *"/"* || "${gateway_host}" == *","* || "${gateway_host}" == *" "* ]]; then
  echo "WORKER_GATEWAY_HOST contains unsupported characters" >&2
  exit 2
fi

mkdir -p "${target_dir}"
chmod 0700 "${target_dir}"

ca_valid=false
if [[ -f "${target_dir}/ca.key" && -f "${target_dir}/ca.crt" ]] &&
  openssl x509 -in "${target_dir}/ca.crt" -noout -text 2>/dev/null | grep -q "CA:TRUE"; then
  ca_valid=true
fi
if [[ "${ca_valid}" != "true" ]]; then
  openssl genrsa -out "${target_dir}/ca.key" 3072
  chmod 0600 "${target_dir}/ca.key"
  ca_config="$(mktemp "${target_dir}/ca-config.XXXXXX")"
  {
    echo "[req]"
    echo "distinguished_name=req_dn"
    echo "[req_dn]"
    echo "[v3_ca]"
    echo "basicConstraints=critical,CA:TRUE"
    echo "keyUsage=critical,keyCertSign,cRLSign,digitalSignature"
    echo "subjectKeyIdentifier=hash"
    echo "authorityKeyIdentifier=keyid:always,issuer"
  } > "${ca_config}"
  openssl req -x509 -new -key "${target_dir}/ca.key" \
    -sha256 -days 3650 -subj "/CN=Portage Engine Worker CA" \
    -config "${ca_config}" -extensions v3_ca \
    -out "${target_dir}/ca.crt"
  rm -f "${ca_config}"
fi

openssl genrsa -out "${target_dir}/server.key" 3072
chmod 0600 "${target_dir}/server.key"
openssl req -new -key "${target_dir}/server.key" \
  -subj "/CN=${gateway_host}" -out "${target_dir}/server.csr"

san_type="DNS"
if [[ "${gateway_host}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ || "${gateway_host}" == *:* ]]; then
  san_type="IP"
fi
extension_file="$(mktemp "${target_dir}/server-ext.XXXXXX")"
trap 'rm -f "${extension_file}" "${target_dir}/server.csr"' EXIT
{
  echo "basicConstraints=critical,CA:FALSE"
  echo "keyUsage=critical,digitalSignature"
  echo "extendedKeyUsage=serverAuth"
  echo "subjectAltName=${san_type}:${gateway_host}"
} > "${extension_file}"

openssl x509 -req -in "${target_dir}/server.csr" \
  -CA "${target_dir}/ca.crt" -CAkey "${target_dir}/ca.key" \
  -CAserial "${target_dir}/ca.srl" -CAcreateserial \
  -days 30 -sha256 -extfile "${extension_file}" \
  -out "${target_dir}/server.crt"
chmod 0644 "${target_dir}/ca.crt" "${target_dir}/server.crt"

echo "Worker PKI ready in ${target_dir}"
echo "Server SAN: ${san_type}:${gateway_host}"
echo "Keep ca.key and server.key mode 0600 and outside source control."
