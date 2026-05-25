#!/usr/bin/env bash
set -euo pipefail

mkdir -p /run/openvswitch /var/log/openvswitch /var/lib/openvswitch/pki

# Start OVSDB server.
ovsdb-server \
  --remote=punix:/run/openvswitch/db.sock \
  --remote=db:Open_vSwitch,Open_vSwitch,manager_options \
  --pidfile=/run/openvswitch/ovsdb-server.pid \
  --log-file=/dev/stdout \
  --detach

# Bootstrap schema (no-op if already initialized).
ovs-vsctl --no-wait init

# Start vswitch daemon (foreground; logs to stdout).
exec ovs-vswitchd \
  unix:/run/openvswitch/db.sock \
  --pidfile=/run/openvswitch/ovs-vswitchd.pid \
  --log-file=/dev/stdout \
  --mlockall
