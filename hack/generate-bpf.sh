#!/usr/bin/env bash
# Regenerates the eBPF objects of the WireGuard demux fast path (bpf2go, both byte orders) in a golang container with clang and
# libbpf, and copies the generated files back into the tree (they are committed, so a build needs no clang).
#   hack/generate-bpf.sh            run it in a golang container (needs docker)
set -euo pipefail

image=${BPF_GEN_IMAGE:-docker.io/golang:1.27.1}
dir=internal/proxy/demux/fastpath
snapshot=$(mktemp -d)
container="cice-bpf-gen-$$-${RANDOM}"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$snapshot"
}
trap cleanup EXIT
# Copy a source snapshot in and the results out: Docker Desktop cannot bind every external volume.
git ls-files --cached --others --exclude-standard -z > "$snapshot/files"
COPYFILE_DISABLE=1 tar --no-xattrs -cf "$snapshot/source.tar" --null -T "$snapshot/files"
docker create --name "$container" -w /src -e GOWORK=off -e DEBIAN_FRONTEND=noninteractive -v cice-netns-gomod:/go/pkg/mod "$image" bash -c '
  set -euo pipefail
  # https: large packages over the plain http mirror are cut off on some networks.
  sed -i "s|http://deb.debian.org|https://deb.debian.org|g" /etc/apt/sources.list.d/debian.sources
  apt-get update -qq && apt-get install -o Acquire::Retries=5 -y -qq --no-install-recommends clang llvm-19 libbpf-dev linux-libc-dev dpkg-dev >/dev/null
  ln -sfn "/usr/include/$(dpkg-architecture -q DEB_HOST_MULTIARCH)/asm" /usr/include/asm
  command -v llvm-strip >/dev/null || ln -sf "$(find /usr/bin -name "llvm-strip-*" | head -1)" /usr/local/bin/llvm-strip
  go generate ./'"$dir" >/dev/null
docker cp - "$container":/src < "$snapshot/source.tar"
docker start -a "$container" || true
code=$(docker inspect -f '{{.State.ExitCode}}' "$container")
[[ "$code" == 0 ]] || exit "$code"
docker cp "$container":/src/$dir/. "$snapshot/out"
cp "$snapshot"/out/wgfastpath_bpfe[lb].* "$dir"/
