#!/bin/bash
# Run full deploy inside Lima VM where Kind cluster lives.
# Usage: ./hack/lima-deploy.sh [make-target]
# Default target: kind-deploy
set -euo pipefail

TARGET="${1:-kind-deploy}"
PROJECT="/projects/cybericebox/laboratory"

limactl shell icebox-dev -- sudo bash -c "
  export PATH=\$PATH:/usr/local/go/bin
  cd ${PROJECT}
  make ${TARGET}
"
