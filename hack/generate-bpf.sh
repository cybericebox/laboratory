#!/usr/bin/env bash
# Regenerates the eBPF objects of the WireGuard demux fast path (bpf2go, both byte orders) in a golang container with clang and
# libbpf, and copies the generated files back into the tree (they are committed, so a build needs no clang). The image is pinned
# by digest and the packages come from the Debian snapshot the image was built from, so the same source gives the same objects
# (CI regenerates and compares: the bpf job of .github/workflows/test.yml).
#   hack/generate-bpf.sh            run it in a golang container (needs docker)
set -euo pipefail

image=${BPF_GEN_IMAGE:-docker.io/golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf}
snapshot_date=20261104T212339Z # the Debian snapshot of that image (/etc/apt/sources.list.d/debian.sources)
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
  cat > /etc/apt/sources.list.d/debian.sources <<SOURCES
Types: deb
URIs: https://snapshot.debian.org/archive/debian/'"$snapshot_date"'
Suites: trixie trixie-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.pgp

Types: deb
URIs: https://snapshot.debian.org/archive/debian-security/'"$snapshot_date"'
Suites: trixie-security
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.pgp
SOURCES
  apt-get -o Acquire::Check-Valid-Until=false -o Acquire::Retries=5 update -qq && apt-get install -o Acquire::Retries=5 -y -qq --no-install-recommends clang llvm-19 libbpf-dev linux-libc-dev dpkg-dev >/dev/null
  ln -sfn "/usr/include/$(dpkg-architecture -q DEB_HOST_MULTIARCH)/asm" /usr/include/asm
  command -v llvm-strip >/dev/null || ln -sf "$(find /usr/bin -name "llvm-strip-*" | head -1)" /usr/local/bin/llvm-strip
  go generate ./'"$dir" >/dev/null
docker cp - "$container":/src < "$snapshot/source.tar"
docker start -a "$container" || true
code=$(docker inspect -f '{{.State.ExitCode}}' "$container")
[[ "$code" == 0 ]] || exit "$code"
docker cp "$container":/src/$dir/. "$snapshot/out"
cp "$snapshot"/out/wgfastpath_bpfe[lb].* "$dir"/
