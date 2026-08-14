#!/usr/bin/env bash
# Reproducible local quality gate. Tool versions intentionally match CI.

set -euo pipefail

readonly golangci_lint_version="v2.12.2"
readonly govulncheck_version="v1.7.0"
readonly golangci_lint_package="github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${golangci_lint_version}"
readonly govulncheck_package="golang.org/x/vuln/cmd/govulncheck@${govulncheck_version}"

run_golangci_lint() {
    go run "${golangci_lint_package}" "$@"
}

echo "=== gofmt ==="
unformatted="$(gofmt -l -- .)"
if [[ -n "${unformatted}" ]]; then
    echo "unformatted Go files:" >&2
    echo "${unformatted}" >&2
    exit 1
fi

echo "=== go vet ==="
go vet ./...

echo "=== golangci-lint ${golangci_lint_version} ==="
run_golangci_lint run --timeout=10m ./...

echo "=== gosec via golangci-lint ${golangci_lint_version} ==="
run_golangci_lint run --enable-only=gosec --timeout=10m ./...

echo "=== complexity via golangci-lint ${golangci_lint_version} ==="
run_golangci_lint run --enable-only=cyclop,gocyclo --timeout=10m ./...

echo "=== govulncheck ${govulncheck_version} ==="
go run "${govulncheck_package}" ./...

echo "=== quality gate passed ==="
