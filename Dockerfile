# syntax=docker/dockerfile:1

# One Dockerfile, three images (build targets): laboratory, node, lab.
#
# Every Laboratory component is one multicall binary, /laboratory. A component is
# chosen by the first argument (`/laboratory manager`), or by the name the binary is
# invoked as (cni-gate on the host).
#
# Layer order in every image: alpine base -> /laboratory -> per-image extras. The
# first two layers are byte-identical across the images (the binary's mtime is fixed),
# so a node that has pulled one image already holds them for the others.
ARG ALPINE=alpine:3.24.2

# The Go stage always runs on the build host and cross-compiles, so multi-arch builds
# do not go through QEMU.
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
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/laboratory ./cmd/laboratory && \
    touch -d @0 /out/laboratory

FROM ${ALPINE} AS base
COPY --from=builder /out/laboratory /laboratory

# CNI plugins for the node image: only the ones the node-agent installs on the host.
FROM ${ALPINE} AS cni
RUN apk add --no-cache cni-plugins

# operator, agent, proxy (Deployments). Needs nothing but the binary and the CA bundle
# that alpine ships.
FROM base AS laboratory
USER 65532:65532

# Per lab group: VPN and gateway pods.
FROM base AS lab
RUN apk add --no-cache iptables iproute2 wireguard-tools-wg

# One pod per node: node-agent, CNI helpers and Open vSwitch (alpine package, kernel datapath).
FROM base AS node
RUN apk add --no-cache openvswitch iproute2 kmod bash util-linux-misc
COPY --from=cni /usr/libexec/cni/bridge /usr/libexec/cni/ptp /usr/libexec/cni/loopback \
     /usr/libexec/cni/host-local /usr/libexec/cni/portmap /usr/libexec/cni/
COPY --chmod=0755 scripts/start-ovs.sh /node-agent/bin/start-ovs.sh
