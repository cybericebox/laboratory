#!/usr/bin/env bash
# P0 spike of the registry gate (docs/research/2026-11-16-zot-tenant-cache.md, sections 9 and 11): does the pull path the design
# relies on work on a real kind node?
#   c1  containerd sends Basic credentials to a plain-HTTP localhost:5035 registry after a 401 challenge
#   c2  the kubelet picks the pull secret entry keyed localhost:5035/c/<registry> (a path prefix), and only that one
#   c3  KEP-2535: a pod with other credentials on a node that already holds the image is verified against the registry
# The registry is a fake (hack/registry-gate-p0/gate) running in the node's network namespace on 127.0.0.1:5035, like the relay;
# its request log is the evidence. Every case reports PASS, FAIL or INFO (an observation the design does not depend on); the
# script exits 1 when a case FAILs, after all of them ran. Needs docker, kind, kubectl, jq, go.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 2

CLUSTER=${P0_CLUSTER:-p0-gate}
NODE_IMAGE=${P0_NODE_IMAGE:-kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0}
VERIFY_POLICY=${P0_VERIFY_POLICY:-NeverVerifyPreloadedImages}
OUT=${P0_OUT:-/tmp/p0}
NODE=$CLUSTER-control-plane
RESULTS=$OUT/results.tsv
mkdir -p "$OUT"
: >"$RESULTS"

say() { printf '%s\n' "$*" >&2; }
record() { printf '%s\t%s\t%s\n' "$1" "$2" "$3" | tee -a "$RESULTS" >&2; }
pass() { record PASS "$1" "$2"; }
fail() { record FAIL "$1" "$2"; }
info() { record INFO "$1" "$2"; }
check() { # check ID "what" <command that succeeds when the claim holds>
  local id=$1 what=$2
  shift 2
  if "$@"; then pass "$id" "$what"; else fail "$id" "$what"; fi
}
node() { docker exec "$NODE" "$@"; }
crictl() { node crictl --runtime-endpoint unix:///run/containerd/containerd.sock "$@"; }

# ---- build and start ---------------------------------------------------------------------------------------------------------
goarch=$(go env GOARCH)
say "== building the fake gate and the image binary (linux/$goarch)"
GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -o "$OUT/gate" ./hack/registry-gate-p0/gate || exit 2
GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -o "$OUT/sleeper" ./hack/registry-gate-p0/sleeper || exit 2

say "== kind cluster $CLUSTER on $NODE_IMAGE (verification policy $VERIFY_POLICY)"
sed "s/__VERIFY_POLICY__/$VERIFY_POLICY/" hack/registry-gate-p0/kind-config.yaml >"$OUT/kind-config.yaml"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config "$OUT/kind-config.yaml" --wait 120s || exit 2

node mkdir -p /opt/p0
docker cp "$OUT/gate" "$NODE:/opt/p0/gate"
docker cp "$OUT/sleeper" "$NODE:/opt/p0/sleeper"
# Credentials: alice and carol may read registry "up", bob registry "other". mallory is nobody.
docker exec -d "$NODE" /opt/p0/gate --listen 127.0.0.1:5035 --sleeper /opt/p0/sleeper \
  --cred 'up=t1~alice:pwA' --cred 'up=t2~carol:pwC' --cred 'other=t1~bob:pwB'
for _ in $(seq 1 30); do
  [ "$(node curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:5035/v2/)" = 200 ] && break
  sleep 1
done
D=$(node cat /var/log/p0-gate.digest)
say "== image digest $D"

LOG=$OUT/gate.log
MARK=0
fetch_log() { node cat /var/log/p0-gate.log >"$LOG" 2>/dev/null || : >"$LOG"; }
mark() { sleep 3; fetch_log; MARK=$(wc -l <"$LOG"); }
since() { sleep 3; fetch_log; tail -n +$((MARK + 1)) "$LOG"; }
# n_since 'jq-condition' counts the requests logged since mark().
n_since() { since | jq -s "map(select($1)) | length"; }
seen() { [ "$(n_since "$1")" -gt 0 ]; }
unseen() { [ "$(n_since "$1")" -eq 0 ]; }

localhost_images() { crictl images -o json 2>/dev/null |
  jq -r '.images[] | select(any(.repoDigests[]?; startswith("localhost:5035/"))) | .id'; }
remove_images() { for id in $(localhost_images); do crictl rmi "$id" >/dev/null 2>&1; done; return 0; }
has_image() { crictl images -o json 2>/dev/null | jq -e --arg p "$1@" 'any(.images[]; any(.repoDigests[]?; startswith($p)))' >/dev/null; }
first_request_is_anonymous() { [ "$(since | jq -s 'map(select(.kind != "ping")) | .[0].auth')" = '"none"' ]; }

mk_secret() { # mk_secret NS NAME 'key|user|pass'...
  local ns=$1 name=$2 auths='{}' spec key user pass
  shift 2
  for spec in "$@"; do
    IFS='|' read -r key user pass <<<"$spec"
    auths=$(jq -c --arg k "$key" --arg u "$user" --arg p "$pass" \
      '.[$k] = {username: $u, password: $p, auth: (($u + ":" + $p) | @base64)}' <<<"$auths")
  done
  kubectl create namespace "$ns" >/dev/null 2>&1 || true
  kubectl -n "$ns" create secret generic "$name" --type=kubernetes.io/dockerconfigjson \
    --from-literal=.dockerconfigjson="$(jq -nc --argjson a "$auths" '{auths: $a}')" >/dev/null
}
mk_pod() { # mk_pod NS NAME SECRET|- IMAGE...
  local ns=$1 name=$2 secret=$3 i=0 containers=''
  shift 3
  kubectl create namespace "$ns" >/dev/null 2>&1 || true
  for img in "$@"; do
    containers+="    - {name: c$i, image: \"$img\", imagePullPolicy: IfNotPresent}"$'\n'
    i=$((i + 1))
  done
  {
    printf 'apiVersion: v1\nkind: Pod\nmetadata: {name: %s, namespace: %s}\nspec:\n' "$name" "$ns"
    printf '  terminationGracePeriodSeconds: 0\n'
    [ "$secret" != - ] && printf '  imagePullSecrets: [{name: %s}]\n' "$secret"
    printf '  containers:\n%s' "$containers"
  } | kubectl apply -f - >/dev/null
}
pod_state() { # running | pullfail | pending
  kubectl -n "$1" get pod "$2" -o json | jq -r '
    if ([.status.containerStatuses[]? | select(.state.running)] | length) == (.spec.containers | length) then "running"
    elif ([.status.containerStatuses[]? | .state.waiting.reason // ""] | any(. == "ErrImagePull" or . == "ImagePullBackOff")) then "pullfail"
    else "pending" end'
}
wait_pod() { # wait_pod NS NAME -> prints the end state (running | pullfail | pending after the timeout)
  local s
  for _ in $(seq 1 60); do
    s=$(pod_state "$1" "$2")
    [ "$s" != pending ] && { echo "$s"; return; }
    sleep 2
  done
  echo pending
}
state_is() { [ "$(wait_pod "$1" "$2")" = "$3" ]; }

# ---- the cluster is what the design assumes -------------------------------------------------------------------------------
minor=$(kubectl version -o json | jq -r '.serverVersion.minor' | tr -dc '0-9')
check k0 "the node runs Kubernetes 1.35 or newer (minor $minor)" test "$minor" -ge 35
gates=$(kubectl get --raw "/api/v1/nodes/$NODE/proxy/configz" | jq -c '.kubeletconfig | {featureGates, imagePullCredentialsVerificationPolicy}')
say "kubelet: $gates"
check k0b "the kubelet runs with KubeletEnsureSecretPulledImages on" \
  test "$(jq -r '.featureGates.KubeletEnsureSecretPulledImages' <<<"$gates")" = true

# ---- c1: containerd, a Basic challenge, plain HTTP on localhost ----------------------------------------------------------
say "== c1"
ref="localhost:5035/c/up/img-c1@$D"
mark
crictl pull --creds 't1~alice:pwA' "$ref" >"$OUT/c1-pull.txt" 2>&1
check c1a "crictl pull with credentials from a plain-HTTP localhost registry succeeds" has_image localhost:5035/c/up/img-c1
check c1b "the first request had no credentials and got 401 (the challenge)" first_request_is_anonymous
check c1c "a manifest request without credentials got 401" \
  seen '.kind == "manifest" and .auth == "none" and .status == 401'
check c1d "containerd then sent Authorization: Basic with the username of the pull (t1~alice) and got 200" \
  seen '.kind == "manifest" and .auth == "basic" and .user == "t1~alice" and .status == 200'
remove_images
mark
crictl pull --creds 't1~alice:wrong' "localhost:5035/c/up/img-c1w@$D" >"$OUT/c1-wrong.txt" 2>&1
wrong_rc=$?
check c1e "a wrong password does not pull" test "$wrong_rc" -ne 0
check c1f "...and the registry saw the refused credentials" \
  seen '.auth == "basic" and .status == 401 and .user == "t1~alice"'
remove_images

# ---- c2: which entry of the pull secret the kubelet uses ----------------------------------------------------------------
say "== c2"
img() { printf 'localhost:5035/c/%s/%s@%s' "$1" "$2" "$D"; }

mk_secret p0-c2a s "localhost:5035/c/up|t1~alice|pwA"
mark
mk_pod p0-c2a p s "$(img up img-c2a)"
check c2a "a secret keyed localhost:5035/c/up (a path prefix) pulls localhost:5035/c/up/... " state_is p0-c2a p running
check c2a2 "the kubelet sent t1~alice, and the 401 came first" \
  seen '.kind == "manifest" and .auth == "basic" and .user == "t1~alice" and .status == 200'
kubectl delete ns p0-c2a --wait=false >/dev/null 2>&1; remove_images

mk_secret p0-c2b s "localhost:5035|t1~alice|pwA"
mark
mk_pod p0-c2b p s "$(img up img-c2b)"
if [ "$(wait_pod p0-c2b p)" = running ]; then info c2b "a secret keyed with the bare host localhost:5035 also works for localhost:5035/c/up/... (not used by the design)"
else info c2b "a secret keyed with the bare host localhost:5035 does NOT work for localhost:5035/c/up/..."; fi
kubectl delete ns p0-c2b --wait=false >/dev/null 2>&1; remove_images

mk_secret p0-c2c s "up|t1~alice|pwA"
mark
mk_pod p0-c2c p s "$(img up img-c2c)"
check c2c "a secret keyed with the bare registry name (up) is not used: no credentials, the pull fails" state_is p0-c2c p pullfail
check c2c2 "...and the registry saw no Basic credentials" unseen '.auth == "basic"'
kubectl delete ns p0-c2c --wait=false >/dev/null 2>&1; remove_images

mk_secret p0-c2d s "localhost:5035/c/up|t1~alice|pwA" "localhost:5035/c/other|t1~bob|pwB"
mark
mk_pod p0-c2d p s "$(img up img-c2d)" "$(img other img-c2d)"
check c2d "one secret with two registries: a pod with an image of each starts" state_is p0-c2d p running
check c2d2 "t1~alice reached only registry up" \
  unseen '.auth == "basic" and .user == "t1~alice" and (.path | startswith("/v2/c/other/"))'
check c2d3 "t1~bob reached only registry other" \
  unseen '.auth == "basic" and .user == "t1~bob" and (.path | startswith("/v2/c/up/"))'
check c2d4 "registry up got alice's credentials" \
  seen '.auth == "basic" and .user == "t1~alice" and .status == 200 and (.path | startswith("/v2/c/up/"))'
check c2d5 "registry other got bob's credentials" \
  seen '.auth == "basic" and .user == "t1~bob" and .status == 200 and (.path | startswith("/v2/c/other/"))' 
kubectl delete ns p0-c2d --wait=false >/dev/null 2>&1; remove_images

# ---- c3: KEP-2535, other credentials on a node that holds the image -----------------------------------------------------
say "== c3"
C3=$(img up img-c3)
mk_secret p0-c3a s "localhost:5035/c/up|t1~alice|pwA"
mark
mk_pod p0-c3a p s "$C3"
check c3a "pod A (alice) pulls the image onto the node" state_is p0-c3a p running
check c3a0 "the image is on the node" has_image localhost:5035/c/up/img-c3

mk_secret p0-c3b s "localhost:5035/c/up|t1~alice|pwA"
mark
mk_pod p0-c3b p s "$C3"
check c3b "pod B1 (the same credentials in another namespace) starts" state_is p0-c3b p running
if unseen '.kind == "manifest" or .kind == "blob"'; then
  pass c3b2 "the same credentials: the kubelet's record is enough, the registry is not asked"
else
  info c3b2 "the same credentials in another namespace: the registry WAS asked ($(n_since '.kind == "manifest"') manifest requests)"
fi

mk_secret p0-c3c s "localhost:5035/c/up|t2~carol|pwC"
mark
mk_pod p0-c3c p s "$C3"
check c3c "pod B2 (other valid credentials, image already on the node) starts" state_is p0-c3c p running
check c3c2 "the registry was asked with carol's credentials although the image was on the node (KEP-2535)" \
  seen '.kind == "manifest" and .auth == "basic" and .user == "t2~carol" and .status == 200'

mk_secret p0-c3d s "localhost:5035/c/up|t3~mallory|pwM"
mark
mk_pod p0-c3d p s "$C3"
check c3d "pod B3 (credentials the registry refuses, image on the node) does NOT start" state_is p0-c3d p pullfail
check c3d2 "the registry was asked with mallory's credentials and refused them" \
  seen '.kind == "manifest" and .auth == "basic" and .user == "t3~mallory" and .status == 401'

mark
mk_pod p0-c3e p - "$C3"
check c3e "pod B4 (no pull secret, image on the node) does NOT start" state_is p0-c3e p pullfail
check c3e2 "the registry was asked without credentials" seen '.kind == "manifest" and .auth == "none" and .status == 401'

# ---- evidence and verdict -----------------------------------------------------------------------------------------------------
fetch_log
node journalctl -u kubelet --no-pager 2>/dev/null | tail -n 400 >"$OUT/kubelet.log" || true
node journalctl -u containerd --no-pager 2>/dev/null | tail -n 400 >"$OUT/containerd.log" || true
kubectl get pods -A -o wide >"$OUT/pods.txt" 2>&1 || true
{
  echo "### Registry gate P0 (kind $NODE_IMAGE, policy $VERIFY_POLICY)"
  echo
  echo "| result | case | what |"
  echo "| --- | --- | --- |"
  awk -F'\t' '{ printf "| %s | %s | %s |\n", $1, $2, $3 }' "$RESULTS"
} >"$OUT/summary.md"
[ -n "${GITHUB_STEP_SUMMARY:-}" ] && cat "$OUT/summary.md" >>"$GITHUB_STEP_SUMMARY"
cat "$OUT/summary.md"
if [ "${P0_KEEP:-}" != 1 ]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
! grep -q '^FAIL' "$RESULTS"
