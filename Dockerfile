# syntax=docker/dockerfile:1

# One Dockerfile, five images (build targets): controller, agent, proxy, node, lab.
# Each image carries only the code of its own domain, on a minimal base pinned by digest:
#   controller  /manager                                  distroless static, non-root, no shell
#   agent       /agent                                    distroless static, non-root, no shell
#   proxy       /proxy  (proxy-l7, proxy-wg)              distroless static, non-root, no shell
#   node        /node   (node-agent, install-cni, netconfig, cni-gate) + Open vSwitch    alpine (root and a shell:
#               it drives OVS and pod networking on the host, and start-ovs.sh and the init scripts are shell)
#   lab         /lab    (vpn, gateway) + the iptables binaries only                     built from scratch: no shell,
#               no package manager (root: the pods need NET_ADMIN, which Kubernetes gives only to root)
# Multi-component binaries pick the component by first argument, or by the name they
# are started as (the host copy of cni-gate).
ARG ALPINE=alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
ARG DISTROLESS=gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

# The Go stage always runs on the build host and cross-compiles, so multi-arch builds
# do not go through QEMU.
FROM --platform=$BUILDPLATFORM docker.io/golang:1.27.1@sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190 AS builder
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
# static; the tarball is verified against checksums kept HERE (not fetched from the same place as the tarball).
FROM ${ALPINE} AS cni
ARG TARGETARCH
ARG CNI_PLUGINS=v1.9.1
ARG CNI_SHA256_AMD64=b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303
ARG CNI_SHA256_ARM64=56171987d3947707c3563db2f4001bccaf50fd63468611b9f3cbecb1375ee7ec
RUN set -eu; \
    base=https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS}; \
    tgz=cni-plugins-linux-${TARGETARCH}-${CNI_PLUGINS}.tgz; \
    case "$TARGETARCH" in amd64) want=$CNI_SHA256_AMD64;; arm64) want=$CNI_SHA256_ARM64;; *) echo "no checksum for $TARGETARCH"; exit 1;; esac; \
    wget -q -O /tmp/$tgz $base/$tgz; \
    echo "$want  /tmp/$tgz" | sha256sum -c -; \
    mkdir -p /cni && tar -xzf /tmp/$tgz -C /cni ./bridge ./ptp ./loopback ./host-local ./portmap; \
    rm -f /tmp/$tgz
# The upstream binaries carry the symbol table and DWARF (about a third of their size); the Go runtime needs neither.
RUN apk add --no-cache binutils && strip --strip-unneeded /cni/*

FROM ${DISTROLESS} AS controller
COPY --from=builder /out/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]

FROM ${DISTROLESS} AS agent
COPY --from=builder /out/agent /agent
USER 65532:65532
ENTRYPOINT ["/agent"]

# Faces the internet: nothing but the binary and the CA bundle.
FROM ${DISTROLESS} AS proxy
COPY --from=builder /out/proxy /proxy
USER 65532:65532

# The VPN and gateway pods run /lab and, through it, the iptables binaries: nothing else. The root file system is built
# from the alpine packages of exactly those and copied onto an empty image, so there is no shell, no package manager and
# no busybox in the final image.
FROM ${ALPINE} AS lab-rootfs
RUN apk add --no-cache --root /rootfs --initdb --no-scripts --keys-dir /etc/apk/keys --repositories-file /etc/apk/repositories iptables ip6tables && \
    rm -rf /rootfs/etc/apk /rootfs/lib/apk /rootfs/var /rootfs/sbin/apk /rootfs/usr/share/apk /rootfs/usr/share/man && \
    rm -rf /rootfs/bin /rootfs/etc/busybox-paths.d /rootfs/etc/network /rootfs/etc/udhcpc /rootfs/etc/logrotate.d /rootfs/etc/securetty /rootfs/usr/share/udhcpc && \
    rm -f /rootfs/usr/sbin/arptables* /rootfs/usr/sbin/ebtables* /rootfs/usr/sbin/*-apply /rootfs/usr/lib/xtables/libarpt_* /rootfs/usr/lib/xtables/libebt_*

FROM scratch AS lab
ENV PATH=/usr/sbin:/usr/bin:/sbin:/bin
COPY --from=lab-rootfs /rootfs/ /
COPY --from=builder /out/lab /lab

# Open vSwitch comes from the alpine package (kernel datapath).
FROM ${ALPINE} AS node
RUN apk add --no-cache openvswitch iproute2 kmod bash util-linux-misc
COPY --from=cni /cni/ /usr/libexec/cni/
COPY --chmod=0755 scripts/start-ovs.sh /node-agent/bin/start-ovs.sh
COPY --from=builder /out/node /node
