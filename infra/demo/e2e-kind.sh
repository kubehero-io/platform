#!/usr/bin/env bash
# End-to-end test of the real thing on a local kind cluster:
# build every image → load into kind → helm install the chart (embedded
# Postgres + ClickHouse, auth on) → run a small real "shop" → wait for
# the collector's real data → assert every signal through the public API.
#
#   ./infra/demo/e2e-kind.sh           # run (keeps the cluster on failure)
#   KEEP=1 ./infra/demo/e2e-kind.sh    # keep the cluster on success too
#   ./infra/demo/e2e-kind.sh --clean   # delete the cluster
#
# Requires: docker, kind, helm, kubectl, curl, jq.

set -euo pipefail

CLUSTER="${CLUSTER:-kubehero-e2e}"
NS="kubehero-system"
TAG="e2e"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHART="$ROOT/deploy/helm/kubehero"
PORT="${PORT:-18080}"

c_reset="\033[0m"; c_g="\033[32m"; c_b="\033[34m"; c_r="\033[31m"; c_d="\033[2m"
step() { printf "${c_b}▸${c_reset} %s\n" "$*"; }
ok()   { printf "${c_g}✓${c_reset} %s\n" "$*"; }
note() { printf "${c_d}  %s${c_reset}\n" "$*"; }
fail() { printf "${c_r}✗ %s${c_reset}\n" "$*"; exit 1; }

if [ "${1:-}" = "--clean" ]; then
  kind delete cluster --name "$CLUSTER"; exit 0
fi
for bin in docker kind helm kubectl curl jq; do
  command -v "$bin" >/dev/null || fail "missing dependency: $bin"
done

# ─── 1. cluster ──────────────────────────────────────────────────────────
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  step "creating kind cluster $CLUSTER"
  cat <<EOF | kind create cluster --name "$CLUSTER" --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    labels: { topology.kubernetes.io/zone: zone-a, kubehero.io/nodepool: general }
  - role: worker
    labels: { topology.kubernetes.io/zone: zone-b, kubehero.io/nodepool: general }
EOF
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

# ─── 2. images ───────────────────────────────────────────────────────────
IMAGES="control-plane:services/control-plane/Dockerfile
collector:services/collector/Dockerfile
operator:services/operator/Dockerfile
advisor:services/advisor/Dockerfile
pricing-engine:services/pricing-engine/Dockerfile
dashboard:apps/dashboard/Dockerfile
demo-generator:infra/demo/generator/Dockerfile"
for entry in $IMAGES; do
  name=${entry%%:*}; dockerfile=${entry#*:}
  step "building kubehero/$name:$TAG"
  docker build -q -f "$ROOT/$dockerfile" -t "kubehero/$name:$TAG" "$ROOT" >/dev/null
  kind load docker-image "kubehero/$name:$TAG" --name "$CLUSTER" >/dev/null
done
ok "images built + loaded"

# ─── 3. chart ────────────────────────────────────────────────────────────
step "installing the chart (embedded stores, auth required)"
# The optional subcharts (all off by default) aren't vendored, and helm
# refuses to install a chart whose declared dependencies are missing.
helm dependency update "$CHART" >/dev/null
helm upgrade --install kubehero "$CHART" -n "$NS" --create-namespace \
  --set image.registry="" --set image.repository=kubehero --set image.tag="$TAG" \
  --set image.pullPolicy=IfNotPresent \
  --set cluster.id=kind-e2e \
  --set controlPlane.replicas=1 \
  --set postgresql.storage.size=1Gi --set clickhouse.storage.size=4Gi \
  --set clickhouse.resources.requests.cpu=200m --set clickhouse.resources.requests.memory=512Mi \
  --set collector.usageInterval=10s --set collector.profiling.interval=20s \
  --wait --timeout 10m
ok "chart installed"
helm test kubehero -n "$NS" --logs >/dev/null && ok "helm test passed" || fail "helm test failed"

# ─── 4. workload ─────────────────────────────────────────────────────────
step "deploying the shop workload"
sed "s#kubehero/demo-generator:e2e#kubehero/demo-generator:$TAG#" "$ROOT/infra/demo/e2e-workloads.yaml" | kubectl apply -f - >/dev/null
kubectl -n shop rollout status deploy/frontend deploy/checkout deploy/payments --timeout=3m >/dev/null
ok "shop running"

# ─── 5. API checks ───────────────────────────────────────────────────────
TOKEN=$(kubectl -n "$NS" get secret kubehero-control-plane -o jsonpath='{.data.admin-token}' | base64 -d)
kubectl -n "$NS" port-forward svc/kubehero-control-plane "$PORT:8080" >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true; [ -n "${KEEP:-}" ] || [ -n "${FAILED:-}" ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1' EXIT
sleep 3

rpc() { # rpc <Service/Method> <json>
  curl -sS -X POST "http://127.0.0.1:$PORT/kubehero.v1.$1" \
    -H "Content-Type: application/json" -H "Connect-Protocol-Version: 1" \
    -H "Authorization: Bearer $TOKEN" -d "$2"
}
eventually() { # eventually <description> <timeout-sec> <command…>
  local what="$1" limit="$2"; shift 2
  local deadline=$(( $(date +%s) + limit ))
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || { FAILED=1; fail "$what (after ${limit}s)"; }
    sleep 10
  done
  ok "$what"
}

[ "$(rpc ControlPlaneService/WhoAmI '{}' | jq -r .role)" = "admin" ] && ok "admin token authenticates" || { FAILED=1; fail "WhoAmI"; }
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$PORT/kubehero.v1.ControlPlaneService/ListClusters" \
  -H "Content-Type: application/json" -H "Connect-Protocol-Version: 1" -d '{}')
[ "$code" = "401" ] && ok "anonymous calls rejected" || { FAILED=1; fail "anonymous call got $code"; }

has_cost()     { rpc CostService/GetAllocation '{"window":"1h","aggregate":["namespace"]}' | jq -e '[.allocations[]? | select(.name=="shop")] | length > 0 and .[0].totalCost > 0'; }
has_logs()     { rpc LogsService/QueryLogs '{"query":"{namespace=\"shop\"}","limit":20}' | jq -e '(.lines // []) | length > 0'; }
has_errors()   { rpc LogsService/QueryLogs '{"query":"sum(count_over_time({namespace=\"shop\"} |= \"request failed\" [5m]))"}' | jq -e '(.series // []) | length > 0'; }
has_usage()    { rpc CostService/ListRightsizing '{"namespace":"shop","window":"1h"}' | jq -e '(.recommendations // []) | length > 0'; }
has_profiles() { rpc ProfilesService/ListProfileTargets '{}' | jq -e '[.targets[]? | select(.namespace=="shop")] | length > 0'; }
has_flows()    { rpc NetworkService/GetServiceMap '{"namespace":"shop"}' | jq -e '(.edges // []) | length > 0 and .source != "demo"'; }

eventually "cost allocation has the shop namespace"       300 has_cost
eventually "container logs are queryable with LogQL"      300 has_logs
eventually "LogQL metric query over error lines"          300 has_errors
eventually "rightsizing has recommendations for shop"     420 has_usage
eventually "profiles collected for shop services"         420 has_profiles
if kubectl -n "$NS" logs ds/kubehero-collector | grep -q '"ebpf'; then
  # Soft check: some kind hosts lack cgroup_skb support. fail() exits,
  # so run it in a subshell to keep going either way.
  ( eventually "eBPF service map has shop edges"          300 has_flows ) \
    || note "no eBPF flows yet (kernel without cgroup_skb?); continuing"
fi

ok "end-to-end checks passed"
note "dashboard: kubectl -n $NS port-forward svc/kubehero-dashboard 3001:3001 (sign in with the admin token)"
