# syntax=docker/dockerfile:1

# Multi-stage: a full toolchain to build, nothing but the binary to run.
#
# The runtime stage is distroless static, which contains no shell, no package
# manager and no libc. That is not minimalism for its own sake: an attacker who
# achieves execution in a container with a shell has a toolbox, and one in a
# container without has a static binary and a read-only filesystem. It also
# means the image has no OS packages to appear in a CVE scan, which removes a
# standing source of noise from the ones that matter.
#
# Sluice is pure Go with no cgo, so a static binary is free.

ARG GO_VERSION=1.23

# --- build ------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src

# Dependencies first, in their own layer, so that a source-only change does not
# re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILDDATE=unknown
ARG TARGETOS
ARG TARGETARCH

# CGO_ENABLED=0 for a static binary; -trimpath so the build is reproducible and
# the image does not carry the builder's directory layout.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILDDATE}" \
      -o /out/sluice ./cmd/sluice

# The healthcheck needs something that can make an HTTP request, and distroless
# has no curl. The gateway's own binary is that something: `sluice serve --check`
# validates configuration, and for liveness we use a tiny purpose-built prober
# rather than adding a shell to the runtime image.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags "-s -w" -o /out/healthcheck ./cmd/healthcheck

# --- runtime ----------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="sluice" \
      org.opencontainers.image.description="An LLM gateway: cost accounting, PII redaction, caching and failover." \
      org.opencontainers.image.source="https://github.com/sluice-gw/sluice" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /out/sluice /usr/local/bin/sluice
COPY --from=build /out/healthcheck /usr/local/bin/healthcheck

# The distroless nonroot user is uid 65532. Stated explicitly so that a
# Kubernetes securityContext can pin runAsUser to the same value.
USER 65532:65532

EXPOSE 8080

# Liveness rather than readiness: an orchestrator restarting a container because
# every upstream is down would be restarting the wrong thing. Use /readyz for
# load-balancer membership.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/usr/local/bin/healthcheck", "http://127.0.0.1:8080/healthz"]

ENTRYPOINT ["/usr/local/bin/sluice"]
CMD ["serve"]
