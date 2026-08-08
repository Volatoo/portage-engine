# Build the operator console. It is a separate stage because npm is a build
# dependency of exactly one thing — the dashboard's embedded frontend — and
# nothing that ships. The lockfile is copied on its own so a source-only change
# reuses the install layer, and `npm ci` (not `npm install`) is what makes the
# resolved tree the same one the lockfile records.
FROM node:22.23.2-bookworm-slim@sha256:f32b81066cde10a75dbac96646099533316d94bac4150c55da1636e1f0ffdc46 AS web-build

WORKDIR /app/web

COPY web/package.json web/package-lock.json ./
RUN npm ci

COPY web/ ./
# Writes to /app/internal/dashboard/webassets/bundle/dist — the layout mirrors
# the repository so the outDir in vite.config.ts needs no container-specific
# override.
RUN npm run build

# Build the control-plane binaries. Package builds are deliberately excluded:
# portage-builder runs only in a disposable native Gentoo root/VM.
FROM golang:1.26.5@sha256:2005724102f45917a63e9d092fc0e4ea56ea575048ce147caad5f5f61502c365 AS go-build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

# After COPY internal, so the freshly built bundle is what go:embed compiles in.
# .dockerignore keeps the host's own dist out of the build context, so this is
# the only way a bundle can get here.
COPY --from=web-build /app/internal/dashboard/webassets/bundle/dist \
     ./internal/dashboard/webassets/bundle/dist

RUN CGO_ENABLED=0 go build -trimpath -o /out/portage-server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -o /out/portage-dashboard ./cmd/dashboard && \
    CGO_ENABLED=0 go build -trimpath -o /out/portage-migrate ./cmd/migrate && \
    CGO_ENABLED=0 go build -trimpath -o /out/portage-signer ./cmd/signer && \
    CGO_ENABLED=0 go build -trimpath -o /out/portage-capacity-actuator ./cmd/capacity-actuator && \
    CGO_ENABLED=0 go build -trimpath -o /out/portage-artifact-lifecycle ./cmd/artifact-lifecycle

FROM hashicorp/terraform:1.15.6@sha256:adae45661e45d3c88beef071ee1277b4621cea73517aae7f0844657c8e85f641 AS terraform

# Minimal common runtime. Production targets below contain one trust-domain
# binary and run as the same unprivileged numeric identity so deliberately
# shared artifact volumes do not require root.
FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS runtime-base

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd --gid 65532 portage-engine && \
    useradd --uid 65532 --gid 65532 --home-dir /var/lib/portage-engine \
      --create-home --shell /usr/sbin/nologin portage-engine && \
    install -d -o 65532 -g 65532 \
      /opt/portage-engine /var/lib/portage-engine /var/log/portage-engine

WORKDIR /opt/portage-engine

FROM runtime-base AS api-runtime
COPY --from=go-build /out/portage-server /usr/local/bin/portage-server
COPY configs ./configs
USER 65532:65532
EXPOSE 8080 9443
CMD ["/usr/local/bin/portage-server"]

FROM runtime-base AS dashboard-runtime
COPY --from=go-build /out/portage-dashboard /usr/local/bin/portage-dashboard
COPY configs ./configs
USER 65532:65532
EXPOSE 8081
CMD ["/usr/local/bin/portage-dashboard"]

FROM runtime-base AS migrate-runtime
COPY --from=go-build /out/portage-migrate /usr/local/bin/portage-migrate
USER 65532:65532
CMD ["/usr/local/bin/portage-migrate"]

FROM runtime-base AS signer-runtime
USER root
RUN apt-get update && \
    apt-get install -y --no-install-recommends gnupg && \
    rm -rf /var/lib/apt/lists/* && \
    install -d -o 65532 -g 65532 /var/lib/portage-signer
COPY --from=go-build /out/portage-signer /usr/local/bin/portage-signer
USER 65532:65532
CMD ["/usr/local/bin/portage-signer"]

FROM runtime-base AS executor-runtime
USER root
RUN apt-get update && \
    apt-get install -y --no-install-recommends bash gnupg openssh-client && \
    rm -rf /var/lib/apt/lists/*
COPY --from=go-build /out/portage-server /usr/local/bin/portage-server
COPY --from=terraform /bin/terraform /usr/local/bin/terraform
COPY configs ./configs
USER 65532:65532
CMD ["/usr/local/bin/portage-server"]

FROM runtime-base AS actuator-runtime
COPY --from=go-build /out/portage-capacity-actuator /usr/local/bin/portage-capacity-actuator
COPY --from=terraform /bin/terraform /usr/local/bin/terraform
USER 65532:65532
CMD ["/usr/local/bin/portage-capacity-actuator"]

FROM runtime-base AS artifact-lifecycle-runtime
COPY --from=go-build /out/portage-artifact-lifecycle /usr/local/bin/portage-artifact-lifecycle
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/portage-artifact-lifecycle"]

# Backward-compatible trusted/LAN image. The development Compose topology uses
# several commands from one image and intentionally retains root plus its
# shell/tooling. Public deployments must select the target-specific stages.
FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS trusted-runtime
RUN apt-get update && \
    apt-get install -y --no-install-recommends bash ca-certificates gnupg openssh-client && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /opt/portage-engine
COPY --from=go-build /out/portage-server /usr/local/bin/portage-server
COPY --from=go-build /out/portage-dashboard /usr/local/bin/portage-dashboard
COPY --from=go-build /out/portage-migrate /usr/local/bin/portage-migrate
COPY --from=go-build /out/portage-signer /usr/local/bin/portage-signer
COPY --from=go-build /out/portage-capacity-actuator /usr/local/bin/portage-capacity-actuator
COPY --from=go-build /out/portage-artifact-lifecycle /usr/local/bin/portage-artifact-lifecycle
COPY --from=terraform /bin/terraform /usr/local/bin/terraform
COPY configs ./configs
COPY scripts/rotating-log-tee.sh /usr/local/bin/rotating-log-tee
EXPOSE 8080 8081

CMD ["/usr/local/bin/portage-server"]
