#!/usr/bin/env bash
# The real-iptables tests of the VPN and gateway pods (internal/vpn, internal/gateway: TestNetns*). They build network namespaces
# and install the pods' rules with the real iptables, so they run in a privileged Linux container, each test binary in a network
# namespace of its own (`unshare -n`), never in the host's.
#   hack/test-netns.sh            run them in a golang container (needs docker)
#   hack/test-netns.sh inside     what runs in the container (or in any disposable Linux box, as root)
set -euo pipefail

if [[ "${1:-}" != "inside" ]]; then
  image=${NETNS_TEST_IMAGE:-golang:1.27}
  exec docker run --rm --privileged -v "$PWD":/src -w /src \
    -v cice-netns-gomod:/go/pkg/mod -v cice-netns-gocache:/root/.cache/go-build \
    "$image" bash hack/test-netns.sh inside
fi

export GOWORK=off CICE_NETNS_TESTS=1
if ! command -v iptables >/dev/null || ! command -v ip >/dev/null || ! command -v ping >/dev/null; then
  apt-get update -qq && apt-get install -y -qq iptables iproute2 iputils-ping >/dev/null
fi
out=$(mktemp -d)
for pkg in vpn gateway accessroute; do
  go test -c -o "$out/$pkg.test" "./internal/$pkg"
done
for pkg in vpn gateway accessroute; do
  # The test binary is the pod: its own network namespace, so the host's rules are never touched.
  (cd "internal/$pkg" && unshare -n "$out/$pkg.test" -test.run 'TestNetns' -test.v -test.timeout 120s)
done
