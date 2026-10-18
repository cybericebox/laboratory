#!/usr/bin/env bash
# The tests of the demux fast path's eBPF programs (BPF_PROG_TEST_RUN on hand-built frames, and live forwarding between network
# namespaces). They load programs and build devices, so they run as root in a privileged Linux container, the test binary in a
# network namespace of its own (`unshare -n`), never in the host's.
#   hack/test-bpf.sh            run them in a golang container (needs docker)
#   hack/test-bpf.sh inside     what runs in the container (or in any disposable Linux box, as root)
set -euo pipefail

if [[ "${1:-}" != "inside" ]]; then
  image=${BPF_TEST_IMAGE:-docker.io/golang:1.27.1}
  snapshot=$(mktemp -d)
  container="cice-bpf-tests-$$-${RANDOM}"
  cleanup() {
    docker rm -f "$container" >/dev/null 2>&1 || true
    rm -rf "$snapshot"
  }
  trap cleanup EXIT
  # Copy an explicit source snapshot: Docker Desktop cannot bind every external volume.
  git ls-files --cached --others --exclude-standard -z > "$snapshot/files"
  COPYFILE_DISABLE=1 tar --no-xattrs -cf "$snapshot/source.tar" --null -T "$snapshot/files"
  docker create --name "$container" --privileged -w /src \
    -v cice-netns-gomod:/go/pkg/mod -v cice-netns-gocache:/root/.cache/go-build \
    "$image" bash hack/test-bpf.sh inside >/dev/null
  docker cp - "$container":/src < "$snapshot/source.tar"
  docker start -a "$container" || true
  exit "$(docker inspect -f '{{.State.ExitCode}}' "$container")"
fi

export GOWORK=off CICE_BPF_TESTS=1
if ! command -v ip >/dev/null; then
  apt-get update -qq && apt-get install -y -qq iproute2 >/dev/null
fi
out=$(mktemp -d)
go test -c -o "$out/fastpath.test" ./internal/proxy/demux/fastpath
cd internal/proxy/demux/fastpath
set +e
unshare -n "$out/fastpath.test" -test.v -test.count=1 -test.timeout 120s 2>&1 | tee "$out/last.log"
status=${PIPESTATUS[0]}
set -e
if grep -Eq '^[[:space:]]*--- SKIP:' "$out/last.log"; then
  echo "Privileged BPF test unexpectedly skipped" >&2
  exit 1
fi
exit "$status"
