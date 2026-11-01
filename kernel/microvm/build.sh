#!/usr/bin/env bash
# Builds the guest kernel of the microVM profile: the Kata Containers kernel build with our fragment added, and a check that every
# option of the fragment is in the resulting .config. Run by .github/workflows/guest-kernel.yml; needs the build tools of a kernel
# build (gcc, make, flex, bison, bc, libelf, libssl).
#
#   KATA_REF        git tag of kata-containers whose kernel config and patches are used
#   KERNEL_VERSION  upstream kernel version (default: the one Kata pins at KATA_REF)
#   OUT             directory for vmlinux, vmlinuz and .config
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
: "${KATA_REF:?set KATA_REF}"
OUT="${OUT:-$PWD/out}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

git clone --depth 1 --branch "$KATA_REF" https://github.com/kata-containers/kata-containers "$work/kata"
kernel_dir="$work/kata/tools/packaging/kernel"
if [[ -z "${KERNEL_VERSION:-}" ]]; then
  KERNEL_VERSION="$(grep -E '^assets:' -A200 "$work/kata/versions.yaml" | grep -E '^\s+kernel:' -A4 | grep -E '^\s+version:' | head -1 | sed -E 's/.*v?([0-9]+\.[0-9]+\.[0-9]+).*/\1/')"
fi
echo "kata $KATA_REF, kernel $KERNEL_VERSION"

# our options go in as a common fragment: the Kata build merges every fragment of configs/fragments/common
install -m 0644 "$here/cybericebox.fragment" "$kernel_dir/configs/fragments/common/cybericebox.conf"

(cd "$kernel_dir" && ./build-kernel.sh -v "$KERNEL_VERSION" -a x86_64 -f -d setup && ./build-kernel.sh -v "$KERNEL_VERSION" -a x86_64 -f -d build)

built="$(ls -d "$kernel_dir"/kata-linux-*/ | head -1)"
missing=0
while IFS= read -r line; do
  [[ "$line" =~ ^CONFIG_[A-Z0-9_]+=y$ ]] || continue
  if ! grep -qx "$line" "$built/.config"; then
    echo "missing in .config: $line" >&2
    missing=1
  fi
done < "$here/cybericebox.fragment"
[[ "$missing" == 0 ]] || { echo "the kernel dropped options of the fragment" >&2; exit 1; }

mkdir -p "$OUT"
cp "$built/vmlinux" "$built/.config" "$OUT/"
cp "$built/arch/x86/boot/bzImage" "$OUT/vmlinuz" 2>/dev/null || true
echo "$KATA_REF $KERNEL_VERSION" > "$OUT/VERSION"
