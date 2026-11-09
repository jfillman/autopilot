# syntax=docker/dockerfile:1.7
# autopilot-gateway, the Go AI gateway (A-9): one static binary on distroless static, nonroot (uid 65532), no shell, the
# same bases as Clearance's image. NOT the deployed image yet: the Python gateway's Dockerfile (../Dockerfile) still is,
# and gateway-image.yaml builds that one. See docs/admin/go-gateway.md.
#
#   docker buildx build -f gateway/Dockerfile --platform linux/amd64,linux/arm64 --build-arg VERSION=gateway-go-v0.0.1 .
#   container build -f gateway/Dockerfile --build-arg VERSION=dev -t autopilot-gateway-go:dev .   # Apple's container CLI
#
# The build context is the repository root (the bundled agents/ come with the image, as in the Python image).
# Reproducible like Clearance's: -trimpath, no VCS stamp, an empty build id, no clock in the binary. Both bases are
# pinned by digest (the multi-arch index, not one arch); bump them together with a PR.

ARG GO_IMAGE=golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd
ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local
WORKDIR /src/gateway
COPY gateway/go.mod gateway/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY gateway/cmd/ cmd/
COPY gateway/internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags "-s -w -buildid= -X main.version=${VERSION}" -o /out/autopilot-gateway ./cmd/autopilot-gateway

FROM ${BASE_IMAGE}
ARG VERSION=dev
LABEL org.opencontainers.image.title="autopilot-gateway" \
      org.opencontainers.image.description="Autopilot's AI gateway in Go: MCP, the model proxy, sessions and budgets, on Clearance" \
      org.opencontainers.image.source="https://github.com/jfillman/autopilot" \
      org.opencontainers.image.version="${VERSION}"
# The image's own version: the start line and the conformance control interface report it.
ENV AUTOPILOT_GATEWAY_VERSION=${VERSION} CLEARANCE_AGENTS_DIR=/app/agents PORT=8080
COPY --from=build /out/autopilot-gateway /autopilot-gateway
COPY agents/*.yaml /app/agents/
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/autopilot-gateway"]
CMD ["serve"]
