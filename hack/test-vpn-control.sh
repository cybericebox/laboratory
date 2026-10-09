#!/usr/bin/env bash
# Run inside the same disposable Linux runner as test-netns.sh.
set -euo pipefail
if [[ "$(uname -s)" != Linux ]]; then
  echo 'Run in the disposable Linux source container (see hack/test-netns.sh).' >&2
  exit 1
fi
go test -race ./internal/vpn/reconciler -run 'TestControl|TestAccessReconcile'
go test ./internal/vpn -run '^$' -bench 'BenchmarkForwardPlan|BenchmarkUnchangedForwardPlan' -benchmem -count 3
