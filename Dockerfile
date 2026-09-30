# syntax=docker/dockerfile:1

# One Dockerfile, five images (build targets): controller, agent, proxy, node, lab.
# Each image carries only the code of its own domain:
#   controller  /manager                                  distroless static
#   agent       /agent                                    distroless static
#   proxy       /proxy  (proxy-l7, proxy-wg)              distroless static
#   node        /node   (node-agent, install-cni, netconfig, cni-gate) + Open vSwitch    alpine
#   lab         /lab    (vpn, gateway) + iptables, iproute2, wg                          alpine
# Multi-component binaries pick the component by first argument, or by the name they
# are started as (the host copy of cni-gate).
ARG ALPINE=alpine:3.24.2
ARG DISTROLESS=gcr.io/distroless/static:nonroot

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
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}
RUN for c in manager agent proxy node lab; do \
      go build -trimpath -ldflags="-s -w" -o /out/$c ./cmd/$c || exit 1; \
    done

# CNI plugins for the node image: only the ones the node-agent installs on the host. They run on
# the HOST, which is glibc, so they must be static: the alpine package builds them against musl
# (they fail there with "fork/exec ...: no such file or directory"). The upstream release is
# static; its checksum is verified.
FROM ${ALPINE} AS cni
ARG TARGETARCH
ARG CNI_PLUGINS=v1.8.0
RUN set -eu; \
    base=https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS}; \
    tgz=cni-plugins-linux-${TARGETARCH}-${CNI_PLUGINS}.tgz; \
    wget -q -O /tmp/$tgz $base/$tgz; wget -q -O /tmp/$tgz.sha256 $base/$tgz.sha256; \
    (cd /tmp && sha256sum -c $tgz.sha256); \
    mkdir -p /cni && tar -xzf /tmp/$tgz -C /cni ./bridge ./ptp ./loopback ./host-local ./portmap

FROM ${DISTROLESS} AS controller
COPY --from=builder /out/manager /manager
ENTRYPOINT ["/manager"]

FROM ${DISTROLESS} AS agent
COPY --from=builder /out/agent /agent
ENTRYPOINT ["/agent"]

# Faces the internet: nothing but the binary and the CA bundle.
FROM ${DISTROLESS} AS proxy
COPY --from=builder /out/proxy /proxy

# Packages first, the binary last: a new release replaces only the small top layer.
FROM ${ALPINE} AS lab
RUN apk add --no-cache iptables iproute2 wireguard-tools-wg
COPY --from=builder /out/lab /lab

# Open vSwitch comes from the alpine package (kernel datapath).
FROM ${ALPINE} AS node
RUN apk add --no-cache openvswitch iproute2 kmod bash util-linux-misc
COPY --from=cni /cni/ /usr/libexec/cni/
COPY --chmod=0755 scripts/start-ovs.sh /node-agent/bin/start-ovs.sh
COPY --from=builder /out/node /node
