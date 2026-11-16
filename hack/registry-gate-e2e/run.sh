#!/usr/bin/env bash
# E2E of the registry gate on a kind node (Kubernetes 1.35): the whole pull path of the image cache, with the real pieces:
#   kubelet and containerd of the node -> localhost:5035 (the relay code of the node-agent, over TLS) -> the gate -> zot / two upstream
#   registries (one open, one with Basic authentication), the registry pod rendered from the chart with the cache on.
#   g1  a public image: no 401 for a pod without credentials, filled once, then served from the cache
#   g2  a private image: refused without credentials, pulled with the tenant~user secret, downloaded from upstream once
#   g3  other credentials (no right upstream) are refused although the image is in the cache
#   g4  the same credentials of another tenant are checked once more, then the decision is reused
# Every case reports PASS or FAIL; the script exits 1 when one FAILs, after all of them ran. Needs docker, kind, kubectl, helm, jq, go.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 2

CLUSTER=${E2E_CLUSTER:-gate-e2e}
NODE_IMAGE=${E2E_NODE_IMAGE:-kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0}
OUT=${E2E_OUT:-/tmp/gate-e2e}
NODE=$CLUSTER-control-plane
NS=laboratory-system
RESULTS=$OUT/results.tsv
mkdir -p "$OUT"
: >"$RESULTS"

record() { printf '%s\t%s\t%s\n' "$1" "$2" "$3" | tee -a "$RESULTS" >&2; }
say() { printf '%s\n' "$*" >&2; }
check() { # check ID "what" cmd...
  local id=$1 what=$2
  shift 2
  if "$@"; then record PASS "$id" "$what"; else record FAIL "$id" "$what"; fi
}
node() { docker exec "$NODE" "$@"; }
crictl() { node crictl --runtime-endpoint unix:///run/containerd/containerd.sock "$@"; }

# ---- build the image, make the cluster ------------------------------------------------------------------------------------------
say "== building the node binary and the relay"
GOOS=linux CGO_ENABLED=0 go build -o "$OUT/node" ./cmd/node || exit 2
GOOS=linux CGO_ENABLED=0 go build -o "$OUT/relay" ./hack/registry-gate-e2e/relay || exit 2
cp hack/registry-gate-e2e/Dockerfile "$OUT/Dockerfile"
docker build -q -t p0e2e/node:e2e "$OUT" >/dev/null || exit 2

say "== kind cluster $CLUSTER on $NODE_IMAGE"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s || exit 2
kind load docker-image p0e2e/node:e2e --name "$CLUSTER" || exit 2

# ---- the upstream registries, seeded -----------------------------------------------------------------------------------------------
HTPASSWD=$(docker run --rm --entrypoint htpasswd httpd:2.4-alpine -Bbn alice pw) || exit 2
kubectl create namespace "$NS" >/dev/null
kubectl -n "$NS" create secret generic upstream-priv-htpasswd --from-literal=htpasswd="$HTPASSWD" >/dev/null
kubectl apply -f hack/registry-gate-e2e/upstream.yaml >/dev/null 2>&1
kubectl -n "$NS" rollout status deploy/upstream-pub deploy/upstream-priv --timeout=180s >/dev/null || exit 2
kubectl -n "$NS" port-forward svc/upstream-pub 15101:5000 >/dev/null 2>&1 &
kubectl -n "$NS" port-forward svc/upstream-priv 15102:5000 >/dev/null 2>&1 &
sleep 3
DIGESTS=$(go run ./hack/registry-gate-e2e/seed --pub localhost:15101 --priv localhost:15102 --user alice --pass pw) || exit 2
PUB=$(jq -r .pub <<<"$DIGESTS")
PRIV=$(jq -r .priv <<<"$DIGESTS")
say "pub $PUB priv $PRIV"

# ---- the registry pod of the chart, cache on, pointing at them -----------------------------------------------------------------
say "== registry (chart, cache on)"
helm template x charts/laboratory --namespace "$NS" \
  --set operator.publicVPNEndpoint=vpn.example.com:51820 --set operator.baseDomain=lab.example.com --set operator.supportEmail=a@example.com \
  --set nodeAgent.image.repository=p0e2e/node --set nodeAgent.image.tag=e2e --set nodeAgent.image.pullPolicy=Never \
  --set registry.cache.enabled=true --set-json 'registry.cache.registries=[]' --set-json 'registry.allowedRegistries=["pub","priv"]' \
  --set 'registry.cache.extraRegistries[0].name=pub' --set "registry.cache.extraRegistries[0].url=http://upstream-pub.$NS.svc:5000" \
  --set 'registry.cache.extraRegistries[1].name=priv' --set "registry.cache.extraRegistries[1].url=http://upstream-priv.$NS.svc:5000" \
  --set registry.gate.resources.requests.cpu=50m \
  -s templates/registry/deployment.yaml -s templates/registry/service.yaml -s templates/registry/configmap.yaml \
  -s templates/registry/gate-configmap.yaml -s templates/registry/secret.yaml -s templates/registry/tls-secret.yaml \
  -s templates/registry/pvc.yaml >"$OUT/registry.yaml" || exit 2
kubectl apply -f "$OUT/registry.yaml" >/dev/null || exit 2
kubectl -n "$NS" rollout status deploy/laboratory-registry --timeout=300s || { kubectl -n "$NS" describe pod -l app=laboratory-registry >&2; exit 2; }
kubectl apply -f hack/registry-gate-e2e/relay-pod.yaml >/dev/null || exit 2
kubectl -n "$NS" wait --for=condition=Ready pod/relay --timeout=120s >/dev/null || exit 2
REG_POD=$(kubectl -n "$NS" get pod -l app=laboratory-registry -o name | head -1)
kubectl -n "$NS" port-forward "$REG_POD" 15002:5002 >/dev/null 2>&1 &
sleep 2

# ---- helpers ------------------------------------------------------------------------------------------------------------------------------
metric() { # metric 'name{labels}' -> the value, 0 when absent
  curl -s localhost:15002/metrics | awk -v m="$1" 'index($0, m " ") == 1 { print $2; f = 1 } END { if (!f) print 0 }' | head -1
}
upstream_gets() { kubectl -n "$NS" logs deploy/"$1" 2>/dev/null | grep -c "GET /v2/$2/blobs/" || true; }
mk_secret() { # mk_secret NS NAME USER PASS
  kubectl create namespace "$1" >/dev/null 2>&1 || true
  local auths
  auths=$(jq -nc --arg u "$3" --arg p "$4" '{auths: {"localhost:5035/c/priv": {username: $u, password: $p, auth: (($u + ":" + $p) | @base64)}}}')
  kubectl -n "$1" create secret generic "$2" --type=kubernetes.io/dockerconfigjson --from-literal=.dockerconfigjson="$auths" >/dev/null
}
mk_pod() { # mk_pod NS NAME IMAGE [SECRET]
  kubectl create namespace "$1" >/dev/null 2>&1 || true
  {
    printf 'apiVersion: v1\nkind: Pod\nmetadata: {name: %s, namespace: %s}\nspec:\n  terminationGracePeriodSeconds: 0\n' "$2" "$1"
    [ -n "${4:-}" ] && printf '  imagePullSecrets: [{name: %s}]\n' "$4"
    printf '  containers:\n    - {name: c, image: "%s", imagePullPolicy: IfNotPresent}\n' "$3"
  } | kubectl apply -f - >/dev/null
}
# pulled: the image reached the node (the container may fail to start: the test images run nothing)
pulled() {
  for _ in $(seq 1 45); do
    local s
    s=$(kubectl -n "$1" get pod "$2" -o json | jq -r '
      if ([.status.containerStatuses[]? | .imageID // ""] | any(. != "")) then "pulled"
      elif ([.status.containerStatuses[]? | .state.waiting.reason // ""] | any(. == "ErrImagePull" or . == "ImagePullBackOff")) then "failed"
      else "pending" end')
    [ "$s" != pending ] && { [ "$s" = pulled ]; return; }
    sleep 2
  done
  return 1
}
not_pulled() {
  for _ in $(seq 1 45); do
    kubectl -n "$1" get pod "$2" -o json | jq -e '[.status.containerStatuses[]? | .state.waiting.reason // ""] | any(. == "ErrImagePull" or . == "ImagePullBackOff")' >/dev/null && return 0
    sleep 2
  done
  return 1
}
clean_node() { # delete the test namespaces' pods and the images of the cache from the node
  for ns in "$@"; do kubectl delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1; done
  for id in $(crictl images -o json 2>/dev/null | jq -r '.images[] | select(any(.repoDigests[]?; startswith("localhost:5035/"))) | .id'); do crictl rmi "$id" >/dev/null 2>&1; done
}
img() { printf 'localhost:5035/c/%s/%s@%s' "$1" "$2" "$3"; }
ge() { [ "$(awk -v a="$1" -v b="$2" 'BEGIN { print (a >= b) ? 1 : 0 }')" = 1 ]; }
gt() { [ "$(awk -v a="$1" -v b="$2" 'BEGIN { print (a > b) ? 1 : 0 }')" = 1 ]; }

# ---- g1: a public image ----------------------------------------------------------------------------------------------------------------
say "== g1"
mk_pod e2e-g1a p "$(img pub team/app "$PUB")"
check g1a "a pod without credentials pulls a public image through the relay, the gate and zot" pulled e2e-g1a p
check g1b "...and it was stored (blobs filled)" gt "$(metric 'registry_gate_fills_total{kind="blob",result="stored"}')" 0
gets=$(upstream_gets upstream-pub team/app)
clean_node e2e-g1a
hits=$(metric 'registry_gate_cache_total{kind="blob",result="hit"}')
mk_pod e2e-g1b p "$(img pub team/app "$PUB")"
check g1c "the image is pulled again after it left the node" pulled e2e-g1b p
check g1d "...from the cache: cache hits grew" gt "$(metric 'registry_gate_cache_total{kind="blob",result="hit"}')" "$hits"
check g1e "...and the upstream registry saw no second download" test "$(upstream_gets upstream-pub team/app)" -eq "$gets"
clean_node e2e-g1b

# ---- g2: a private image ----------------------------------------------------------------------------------------------------------------
say "== g2"
mk_pod e2e-g2a p "$(img priv team/secret "$PRIV")"
check g2a "a private image without credentials is not pulled (401, containerd has nothing to answer with)" not_pulled e2e-g2a p
mk_secret e2e-g2b s 't1~alice' pw
mk_pod e2e-g2b p "$(img priv team/secret "$PRIV")" s
check g2b "with the secret keyed localhost:5035/c/priv (t1~alice) the image is pulled" pulled e2e-g2b p
gets=$(upstream_gets upstream-priv team/secret)
check g2c "the upstream registry served its blobs" test "$gets" -ge 1
clean_node e2e-g2a e2e-g2b
mk_secret e2e-g2c s 't1~alice' pw
mk_pod e2e-g2c p "$(img priv team/secret "$PRIV")" s
check g2d "pulled again from the cache" pulled e2e-g2c p
check g2e "...without a second download from upstream" test "$(upstream_gets upstream-priv team/secret)" -eq "$gets"
clean_node e2e-g2c

# ---- g3: other credentials, same repository, the blobs are in the cache -----------------------------------------------------------
say "== g3"
denied=$(metric 'registry_gate_upstream_checks_total{outcome="denied"}')
mk_secret e2e-g3 s 't2~bob' wrong
mk_pod e2e-g3 p "$(img priv team/secret "$PRIV")" s
check g3a "credentials with no right upstream are refused although the image is in the cache" not_pulled e2e-g3 p
check g3b "...and the upstream registry refused checks (the counter grew)" gt "$(metric 'registry_gate_upstream_checks_total{outcome="denied"}')" "$denied"
clean_node e2e-g3

# ---- g4: the same credentials of another tenant ---------------------------------------------------------------------------------
say "== g4"
allowed=$(metric 'registry_gate_upstream_checks_total{outcome="allowed"}')
mk_secret e2e-g4a s 't3~alice' pw
mk_pod e2e-g4a p "$(img priv team/secret "$PRIV")" s
check g4a "the same credentials of another tenant are accepted" pulled e2e-g4a p
check g4b "...after a fresh check of their own" gt "$(metric 'registry_gate_upstream_checks_total{outcome="allowed"}')" "$allowed"
clean_node e2e-g4a
allowed=$(metric 'registry_gate_upstream_checks_total{outcome="allowed"}')
mk_secret e2e-g4b s 't3~alice' pw
mk_pod e2e-g4b p "$(img priv team/secret "$PRIV")" s
check g4c "the same tenant again" pulled e2e-g4b p
check g4d "...inside the decision max-age no new check goes upstream" test "$(metric 'registry_gate_upstream_checks_total{outcome="allowed"}')" = "$allowed"

# ---- evidence and verdict -----------------------------------------------------------------------------------------------------------
curl -s localhost:15002/metrics >"$OUT/gate-metrics.txt" 2>/dev/null || true
kubectl -n "$NS" logs "$REG_POD" -c gate >"$OUT/gate.log" 2>&1 || true
kubectl -n "$NS" logs relay >"$OUT/relay.log" 2>&1 || true
kubectl -n "$NS" logs deploy/upstream-priv >"$OUT/upstream-priv.log" 2>&1 || true
kubectl -n "$NS" logs deploy/upstream-pub >"$OUT/upstream-pub.log" 2>&1 || true
node journalctl -u kubelet --no-pager 2>/dev/null | tail -n 400 >"$OUT/kubelet.log" || true
node journalctl -u containerd --no-pager 2>/dev/null | tail -n 400 >"$OUT/containerd.log" || true
kubectl get pods -A -o wide >"$OUT/pods.txt" 2>&1 || true
{
  echo "### Registry gate E2E (kind $NODE_IMAGE)"
  echo
  echo "| result | case | what |"
  echo "| --- | --- | --- |"
  awk -F'\t' '{ printf "| %s | %s | %s |\n", $1, $2, $3 }' "$RESULTS"
} >"$OUT/summary.md"
[ -n "${GITHUB_STEP_SUMMARY:-}" ] && cat "$OUT/summary.md" >>"$GITHUB_STEP_SUMMARY"
cat "$OUT/summary.md"
jobs -p | xargs -r kill 2>/dev/null || true
if [ "${E2E_KEEP:-}" != 1 ]; then kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
! grep -q '^FAIL' "$RESULTS"
