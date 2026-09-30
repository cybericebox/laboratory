#!/usr/bin/env bash
# Runs ovsdb-server and ovs-vswitchd in one container (the node-agent DaemonSet "ovs"
# sidecar). The kernel module is loaded by the host-prep init container; conf.db lives on a
# hostPath so bridges survive pod restarts. Either daemon dying ends the container.
set -euo pipefail

RUN_DIR=/run/openvswitch
DB=/etc/openvswitch/conf.db
SCHEMA=/usr/share/openvswitch/vswitch.ovsschema
SOCK="${OVS_SOCK:-$RUN_DIR/db.sock}"

mkdir -p "$RUN_DIR" /etc/openvswitch /var/log/openvswitch

# RUN_DIR is a hostPath: pid and control files of the previous container are still there.
rm -f "$RUN_DIR"/*.pid "$RUN_DIR"/*.ctl "$SOCK"

if [ ! -f "$DB" ]; then
    ovsdb-tool create "$DB" "$SCHEMA"
elif [ "$(ovsdb-tool needs-conversion "$DB" "$SCHEMA")" = yes ]; then
    ovsdb-tool convert "$DB" "$SCHEMA"
fi

ovsdb-server "$DB" \
  --remote="punix:$SOCK" \
  --remote=db:Open_vSwitch,Open_vSwitch,manager_options \
  --pidfile="$RUN_DIR/ovsdb-server.pid" \
  --log-file=/dev/stdout &

for _ in $(seq 1 50); do
    [ -S "$SOCK" ] && break
    sleep 0.2
done
[ -S "$SOCK" ] || { echo "ovsdb-server did not create $SOCK" >&2; exit 1; }

ovs-vsctl --no-wait init

ovs-vswitchd "unix:$SOCK" \
  --pidfile="$RUN_DIR/ovs-vswitchd.pid" \
  --log-file=/dev/stdout \
  --mlockall &

stop() {
    ovs-appctl -t ovs-vswitchd exit || true
    ovs-appctl -t ovsdb-server exit || true
}
trap stop TERM INT

status=0
wait -n || status=$?
stop
exit "$status"
