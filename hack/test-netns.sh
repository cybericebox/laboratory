#!/usr/bin/env bash
# The real network tests of VPN, gateway, DHCP, access routes, CNI and Node Agent (TestNetns*). They build network namespaces
# and install the pods' rules with the real iptables, so they run in a privileged Linux container, each test binary in a network
# namespace of its own (`unshare -n`), never in the host's.
#   hack/test-netns.sh            run them in a golang container (needs docker)
#   hack/test-netns.sh inside     what runs in the container (or in any disposable Linux box, as root)
set -euo pipefail

if [[ "${1:-}" != "inside" ]]; then
  image=${NETNS_TEST_IMAGE:-golang:1.27}
  snapshot=$(mktemp -d)
  container="cice-netns-tests-$$-${RANDOM}"
  cleanup() {
    docker rm -f "$container" >/dev/null 2>&1 || true
    rm -rf "$snapshot"
  }
  trap cleanup EXIT
  # Copy an explicit source snapshot: Docker Desktop cannot bind every external
  # volume, and tests should never depend on host sharing configuration.
  git ls-files --cached --others --exclude-standard -z > "$snapshot/files"
  COPYFILE_DISABLE=1 tar --no-xattrs -cf "$snapshot/source.tar" --null -T "$snapshot/files"
  docker create --name "$container" --privileged -w /src \
    -v cice-netns-gomod:/go/pkg/mod -v cice-netns-gocache:/root/.cache/go-build \
    "$image" bash hack/test-netns.sh inside >/dev/null
  docker cp - "$container":/src < "$snapshot/source.tar"
  docker start -a "$container"
  exit "$(docker inspect -f '{{.State.ExitCode}}' "$container")"
fi

export GOWORK=off CICE_NETNS_TESTS=1
if ! command -v iptables >/dev/null || ! command -v ip >/dev/null || ! command -v ping >/dev/null || ! command -v conntrack >/dev/null || ! command -v ovsdb-server >/dev/null || ! command -v ovsdb-tool >/dev/null || ! command -v ovs-vsctl >/dev/null; then
  apt-get update -qq && apt-get install -y -qq iptables iproute2 iputils-ping conntrack openvswitch-switch >/dev/null
fi
out=$(mktemp -d)
pkgs="vpn vpn/reconciler gateway accessroute cmds/cnigate nodeagent"
for pkg in $pkgs; do
  go test -c -o "$out/${pkg//\//_}.test" "./internal/$pkg"
done
go test -c -o "$out/dhcp.test" ./pkg/dhcp
run_netns() {
  unshare -n "$1" -test.run TestNetns -test.v -test.timeout 120s 2>&1 | tee "$out/last.log"
  if grep -Eq '^[[:space:]]*--- SKIP:' "$out/last.log"; then
    echo "Privileged network test unexpectedly skipped" >&2
    return 1
  fi
}
run_netns "$out/dhcp.test"
for pkg in $pkgs; do
  # The test binary is the pod: its own network namespace, so the host's rules are never touched.
  (cd "internal/$pkg" && run_netns "$out/${pkg//\//_}.test")
done
