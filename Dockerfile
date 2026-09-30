# syntax=docker/dockerfile:1

# Shared image for the Go services that run as plain Deployments: the operator
# (/manager), the agent (/agent) and the proxy (/proxy-l7, /proxy-wg). Each
# Deployment selects its binary through `command`.
# The Go stage always runs on the build host and cross-compiles, so multi-arch
# builds do not go through QEMU.
FROM --platform=$BUILDPLATFORM docker.io/golang:1.27.1 AS builder
ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY api/ api/
COPY clientset/ clientset/
COPY internal/ internal/
COPY pkg/ pkg/

ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}
RUN go build -trimpath -o /out/manager  ./cmd/main.go && \
    go build -trimpath -o /out/agent    ./cmd/agent && \
    go build -trimpath -o /out/proxy-l7 ./cmd/proxy-l7 && \
    go build -trimpath -o /out/proxy-wg ./cmd/proxy-wg

# Distroless static carries CA certificates, which proxy-l7 needs; none of the
# binaries needs a shell or other tools.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /out/ /
USER 65532:65532
