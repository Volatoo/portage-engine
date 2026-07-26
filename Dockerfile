# Build the control-plane binaries. Package builds are deliberately excluded:
# portage-builder runs only in a disposable native Gentoo root/VM.
FROM golang:1.26.5 AS go-build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

RUN CGO_ENABLED=0 go build -trimpath -o /out/portage-server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -o /out/portage-dashboard ./cmd/dashboard && \
    CGO_ENABLED=0 go build -trimpath -o /out/portage-migrate ./cmd/migrate && \
    CGO_ENABLED=0 go build -trimpath -o /out/portage-signer ./cmd/signer

FROM hashicorp/terraform:1.15.6 AS terraform

# Control-plane runtime only. SSH and GnuPG support PVE deployment and signing;
# Terraform is pinned and copied from HashiCorp's multi-architecture image so
# PVE/cloud provisioning works identically on amd64 and arm64 control planes.
FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends bash ca-certificates gnupg openssh-client && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /opt/portage-engine

COPY --from=go-build /out/portage-server /usr/local/bin/portage-server
COPY --from=go-build /out/portage-dashboard /usr/local/bin/portage-dashboard
COPY --from=go-build /out/portage-migrate /usr/local/bin/portage-migrate
COPY --from=go-build /out/portage-signer /usr/local/bin/portage-signer
COPY --from=terraform /bin/terraform /usr/local/bin/terraform
COPY configs ./configs
COPY scripts/rotating-log-tee.sh /usr/local/bin/rotating-log-tee

EXPOSE 8080 8081

CMD ["/usr/local/bin/portage-server"]
