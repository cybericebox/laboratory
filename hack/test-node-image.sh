#!/usr/bin/env sh
# Smoke a locally built Node image in one disposable privileged container.
# Usage: hack/test-node-image.sh IMAGE[:TAG]
# On an emulated architecture only: NODE_IMAGE_SKIP_OVS_DATAPATH=1 skips the
# OVS kernel datapath probe explicitly; executable/namespace/signal probes remain.
# No host mounts, image pulls, module loads or image/cache deletion.
set -eu
if [ "${NODE_IMAGE_SMOKE_INSIDE:-}" != 1 ]; then
 test "$#" = 1 || { echo "usage: $0 IMAGE[:TAG]" >&2; exit 2; }
 image=$1
 docker image inspect "$image" >/dev/null
 container="cice-node-smoke-$$-$(date +%s)"
 cleanup() { docker rm -f "$container" >/dev/null 2>&1 || true; }
 trap cleanup EXIT
 trap 'exit 130' INT
 trap 'exit 143' TERM
 platform=$(docker image inspect -f '{{.Os}}/{{.Architecture}}' "$image")
 echo "NODE_IMAGE=$image PLATFORM=$platform"
 docker create --name "$container" --privileged --platform "$platform" \
  -e NODE_IMAGE_SMOKE_INSIDE=1 \
  -e NODE_IMAGE_SKIP_OVS_DATAPATH="${NODE_IMAGE_SKIP_OVS_DATAPATH:-0}" \
  --entrypoint /bin/sh "$image" /tmp/node-image-smoke.sh >/dev/null
 docker cp "$0" "$container":/tmp/node-image-smoke.sh
 docker start -a "$container"
 status=$(docker inspect -f '{{.State.ExitCode}}' "$container")
 exit "$status"
fi
# Detect accidental COPY-through-symlink corruption of Alpine's BusyBox.
test "$(/bin/busybox echo busybox-ok)" = busybox-ok
/bin/busybox sleep 0
nsenter --env --version
uname -m
for b in ip nsenter modprobe bash ovsdb-tool ovsdb-server ovs-vswitchd ovs-vsctl ovs-appctl; do command -v "$b"; done
for b in ovsdb-tool ovsdb-server ovs-vswitchd ovs-vsctl ovs-appctl; do "$b" --version | head -1; done
ip -Version
nsenter --version
modprobe --version
ldd /usr/bin/nsenter
test -s /usr/share/openvswitch/vswitch.ovsschema
/node netconfig
mkdir -p /tmp/cni-config
printf '%s\n' '{"cniVersion":"1.0.0","name":"base","type":"bridge"}' > /tmp/cni-config/10-base.conf
CNI_CONF_DIR=/tmp/cni-config GRPC_SOCK=/tmp/node.sock /node install-cni
test -s /tmp/cni-config/00-cybericebox.conflist
cp /node /tmp/cni-gate
for b in /tmp/cni-gate /usr/libexec/cni/bridge /usr/libexec/cni/ptp /usr/libexec/cni/loopback /usr/libexec/cni/host-local /usr/libexec/cni/portmap; do
 CNI_COMMAND=VERSION "$b"
done
mkdir -p /tmp/node-audit-dir
chown -R 65532:65532 /tmp/node-audit-dir
test "$(stat -c %u /tmp/node-audit-dir)" = 65532
# The target runtime's namespace helper and full ip binary, on owned netns only.
ip netns add node-audit
ip link add audit-a type veth peer name audit-b
ip link set audit-b netns node-audit
nsenter --net=/run/netns/node-audit -- ip link set audit-b name eth1
nsenter --net=/run/netns/node-audit -- ip link set eth1 address 02:00:00:00:00:01
nsenter --net=/run/netns/node-audit -- ip link set eth1 up
nsenter --net=/run/netns/node-audit -- ip -j link show eth1
NETCONFIG='[{"name":"eth1","mode":"static","ip":"192.0.2.2/24"}]' \
 nsenter --net=/run/netns/node-audit -- /node netconfig
nsenter --net=/run/netns/node-audit -- ip -j address show eth1
printf '%s\n' '{"cniVersion":"1.0.0","name":"loopback-test","type":"loopback"}' | \
 CNI_COMMAND=ADD CNI_CONTAINERID=smoke CNI_NETNS=/run/netns/node-audit CNI_IFNAME=lo CNI_PATH=/usr/libexec/cni /usr/libexec/cni/loopback
printf '%s\n' '{"cniVersion":"1.0.0","name":"loopback-test","type":"loopback"}' | \
 CNI_COMMAND=DEL CNI_CONTAINERID=smoke CNI_NETNS=/run/netns/node-audit CNI_IFNAME=lo CNI_PATH=/usr/libexec/cni /usr/libexec/cni/loopback
nsenter --net=/run/netns/node-audit -- cat /proc/sys/net/ipv4/ip_forward
nsenter --net=/run/netns/node-audit -- sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward'
ip link del audit-a
ip netns del node-audit
# Read-only module availability/closure; do not load modules into shared Docker VM.
for m in openvswitch geneve wireguard br_netfilter nf_conntrack nf_conntrack_netlink; do
 if test -d /sys/module/$m; then echo "MODULE_ALREADY_PRESENT=$m"; else echo "MODULE_NOT_EXPOSED=$m"; fi
 modprobe -n -v "$m" || echo "SKIP_MODULE_LOAD_REQUIRES_HOST_LIB_MODULES=$m"
done
alive() {
 test -r /proc/$1/stat || return 1
 state=$(sed "s/.*) //" /proc/$1/stat | cut -d " " -f 1)
 test "$state" != Z
}
ready() {
 for i in $(seq 1 100); do
  if ovs-vsctl --timeout=1 --no-wait show >/dev/null 2>&1 && ovs-appctl -t ovs-vswitchd version >/dev/null 2>&1; then return 0; fi
  sleep .1
 done
 return 1
}
/node-agent/bin/start-ovs.sh >/tmp/ovs-first.log 2>&1 & sup=$!
ready
if [ "${NODE_IMAGE_SKIP_OVS_DATAPATH:-0}" = 1 ]; then
 echo 'SKIP: OVS kernel datapath explicitly disabled (emulated architecture limitation)'
else
 ovs-vsctl --timeout=10 add-br audit-br
 ip link show audit-br
fi
ovs-appctl -t ovs-vswitchd version
dbpid=$(cat /run/openvswitch/ovsdb-server.pid)
ovs-appctl -t ovs-vswitchd exit
for i in $(seq 1 100); do alive "$sup" || break; sleep .1; done
if alive "$sup"; then echo SUPERVISOR_DID_NOT_EXIT; exit 1; fi
wait "$sup"
if alive "$dbpid"; then echo DATABASE_ORPHANED; exit 1; fi
echo CHILD_EXIT_SUPERVISION_PASS
/node-agent/bin/start-ovs.sh >/tmp/ovs-second.log 2>&1 & sup=$!
ready
dbpid=$(cat /run/openvswitch/ovsdb-server.pid)
vpid=$(cat /run/openvswitch/ovs-vswitchd.pid)
kill -TERM "$sup"
code=0
wait "$sup" || code=$?
test "$code" = 143
for i in $(seq 1 100); do
 if ! alive "$dbpid" && ! alive "$vpid"; then echo TERM_SUPERVISION_PASS; exit 0; fi
 sleep .1
done
echo SHUTDOWN_LEFT_CHILDREN
exit 1
