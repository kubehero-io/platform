#!/usr/bin/env bash
# End-to-end test of the real thing on a local kind cluster:
# build every image → load into kind → helm install the chart (embedded
# Postgres + ClickHouse, auth on) → run a small real "shop" → wait for
# the collector's real data → assert every signal and product path
# through the public APIs: cost, logs, profiles and eBPF flows; the
# compatibility APIs (Loki, OTLP, OpenCost, FOCUS); alerting; the
# operator's guarded rightsizing (recommend → shadow → armed apply →
# undo); the advisor; the CLI and its MCP server; the dashboard.
#
#   ./infra/demo/e2e-kind.sh           # run (keeps the cluster on failure)
#   KEEP=1 ./infra/demo/e2e-kind.sh    # keep the cluster on success too
#   ./infra/demo/e2e-kind.sh --clean   # delete the cluster
#
#   E2E_UPGRADE_FROM=0.3.1   install that published release first, give it
#                            real data, then upgrade to the chart under test
#   E2E_REQUIRE_EBPF=1       fail (instead of warn) when eBPF flows are missing
#
# Requires: docker, kind, helm, kubectl, curl, jq; go for the CLI checks
# (skipped without it).

set -euo pipefail

CLUSTER="${CLUSTER:-kubehero-e2e}"
NS="kubehero-system"
TAG="e2e"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHART="$ROOT/deploy/helm/kubehero"
PORT="${PORT:-18080}"   # control plane
APORT="${APORT:-18083}" # advisor
DPORT="${DPORT:-13001}" # dashboard

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

# The cluster is kept for diagnostics unless the run reaches the end.
FAILED=1
WORK="$(mktemp -d)"
PFS=""
cleanup() {
  for p in $PFS; do kill "$p" 2>/dev/null || true; done
  rm -rf "$WORK"
  [ -n "${KEEP:-}" ] || [ -n "$FAILED" ] || kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ─── helpers ─────────────────────────────────────────────────────────────
pf() { # pf <service> <local-port> <remote-port>: port-forward, wait until it answers
  kubectl -n "$NS" port-forward "svc/$1" "$2:$3" >/dev/null 2>&1 &
  PFS="$PFS $!"
  for _ in $(seq 1 30); do curl -s -o /dev/null "http://127.0.0.1:$2/" && return 0; sleep 1; done
  fail "port-forward to $1 never answered"
}
pf_stop() { for p in $PFS; do kill "$p" 2>/dev/null || true; done; PFS=""; }
rpc() { # rpc <Service/Method> <json>: control-plane Connect call as admin
  curl -sS --max-time 60 -X POST "http://127.0.0.1:$PORT/kubehero.v1.$1" \
    -H "Content-Type: application/json" -H "Connect-Protocol-Version: 1" \
    -H "Authorization: Bearer $TOKEN" -d "$2"
}
api() { curl -fsS --max-time 60 -H "Authorization: Bearer $TOKEN" "$@"; } # the plain-HTTP faces
eventually() { # eventually <description> <timeout-sec> <command…>
  local what="$1" limit="$2"; shift 2
  local deadline=$(( $(date +%s) + limit ))
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "$what (after ${limit}s)"
    sleep 10
  done
  ok "$what"
}
admin_token() { kubectl -n "$NS" get secret kubehero-control-plane -o jsonpath='{.data.admin-token}' | base64 -d; }
deploy_shop() {
  sed "s#kubehero/demo-generator:e2e#kubehero/demo-generator:$TAG#" "$ROOT/infra/demo/e2e-workloads.yaml" | kubectl apply -f - >/dev/null
  kubectl -n shop rollout status deploy/frontend deploy/checkout deploy/payments --timeout=3m >/dev/null
}
checkout_cpu() { kubectl -n shop get deploy checkout -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}'; }

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
SIZING=(
  --set cluster.id=kind-e2e
  --set controlPlane.replicas=1
  --set postgresql.storage.size=1Gi --set clickhouse.storage.size=4Gi
  --set clickhouse.resources.requests.cpu=200m --set clickhouse.resources.requests.memory=512Mi
  --set collector.usageInterval=10s --set collector.profiling.interval=20s
)
has_cost() { rpc CostService/GetAllocation '{"window":"1h","aggregate":["namespace"]}' | jq -e '[.allocations[]? | select(.name=="shop")] | length > 0 and .[0].totalCost > 0'; }

if [ -n "${E2E_UPGRADE_FROM:-}" ]; then
  # Upgrade path: the published release first, with real data, then the
  # chart under test on top. Tokens and data must survive.
  step "installing the published chart $E2E_UPGRADE_FROM (upgrade baseline)"
  helm install kubehero oci://ghcr.io/kubehero-io/charts/kubehero --version "$E2E_UPGRADE_FROM" \
    -n "$NS" --create-namespace "${SIZING[@]}" --wait --timeout 10m >/dev/null
  ok "baseline $E2E_UPGRADE_FROM installed"
  deploy_shop
  ok "shop running on the baseline"
  TOKEN=$(admin_token)
  TOKEN_BEFORE="$TOKEN"
  pf kubehero-control-plane "$PORT" 8080
  eventually "baseline $E2E_UPGRADE_FROM prices the shop" 300 has_cost
  pf_stop
fi

step "installing the chart under test (embedded stores, auth required)"
# The optional subcharts (all off by default) aren't vendored, and helm
# refuses to install a chart whose declared dependencies are missing.
helm dependency update "$CHART" >/dev/null
helm upgrade --install kubehero "$CHART" -n "$NS" --create-namespace \
  --set image.registry="" --set image.repository=kubehero --set image.tag="$TAG" \
  --set image.pullPolicy=IfNotPresent "${SIZING[@]}" \
  --wait --timeout 10m >/dev/null
ok "chart installed"
helm test kubehero -n "$NS" --logs >/dev/null && ok "helm test passed" || fail "helm test failed"
TOKEN=$(admin_token)
if [ -n "${E2E_UPGRADE_FROM:-}" ]; then
  [ "$TOKEN" = "$TOKEN_BEFORE" ] && ok "upgrade kept the generated admin token" || fail "upgrade rotated the admin token"
fi

# ─── 4. workload ─────────────────────────────────────────────────────────
step "deploying the shop workload"
deploy_shop
ok "shop running"

# ─── 5. signals ──────────────────────────────────────────────────────────
pf kubehero-control-plane "$PORT" 8080

[ "$(rpc ControlPlaneService/WhoAmI '{}' | jq -r .role)" = "admin" ] && ok "admin token authenticates" || fail "WhoAmI"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$PORT/kubehero.v1.ControlPlaneService/ListClusters" \
  -H "Content-Type: application/json" -H "Connect-Protocol-Version: 1" -d '{}')
[ "$code" = "401" ] && ok "anonymous calls rejected" || fail "anonymous call got $code"

has_logs()     { rpc LogsService/QueryLogs '{"query":"{namespace=\"shop\"}","limit":20}' | jq -e '(.lines // []) | length > 0'; }
has_errors()   { rpc LogsService/QueryLogs '{"query":"sum(count_over_time({namespace=\"shop\"} |= \"request failed\" [5m]))"}' | jq -e '(.series // []) | length > 0'; }
has_usage()    { rpc CostService/ListRightsizing '{"namespace":"shop","window":"1h"}' | jq -e '(.recommendations // []) | length > 0'; }
has_profiles() { rpc ProfilesService/ListProfileTargets '{}' | jq -e '[.targets[]? | select(.namespace=="shop")] | length > 0'; }
has_flows()    { rpc NetworkService/GetServiceMap '{"namespace":"shop"}' | jq -e '.source != "demo" and ((.edges // []) | length > 0)'; }
has_ebpf_cpu() { rpc ProfilesService/ListProfileTargets '{}' | jq -e '[.targets[]? | select(.namespace == "shop" and .origin == "ebpf")] | length > 0'; }

eventually "cost allocation has the shop namespace"       300 has_cost
eventually "container logs are queryable with LogQL"      300 has_logs
eventually "LogQL metric query over error lines"          300 has_errors
eventually "rightsizing has recommendations for shop"     420 has_usage
eventually "profiles collected for shop services"         420 has_profiles
ebpf_diag() { # what the flow path saw, when the service map stays empty
  note "service map (shop): $(rpc NetworkService/GetServiceMap '{"namespace":"shop"}' | head -c 800)"
  note "service map (all):  $(rpc NetworkService/GetServiceMap '{}' | jq -c '{source, nodes: (.nodes // [] | length), edges: (.edges // [] | length)}')"
  kubectl -n "$NS" exec kubehero-clickhouse-0 -- sh -c 'clickhouse-client --user "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" -d "$CLICKHOUSE_DB" -q "
    SELECT direction, src_kind, src_namespace, src_workload, dst_kind, dst_namespace,
           dst_workload, dst_service, port, sum(bytes) AS b, count() AS n
    FROM net_flows WHERE ts > now() - INTERVAL 30 MINUTE
    GROUP BY ALL ORDER BY b DESC LIMIT 25 FORMAT PrettyCompactMonoBlock"' || true
  for p in $(kubectl -n "$NS" get pods -l app.kubernetes.io/component=collector -o name); do
    note "${p#pod/}: $(kubectl get --raw "/api/v1/namespaces/$NS/pods/${p#pod/}:8081/proxy/metrics" 2>&1 \
      | grep -E '^kubehero_collector_ebpf|flows' | tr '\n' ' ' | head -c 900)"
  done
}
if [ -n "${E2E_REQUIRE_EBPF:-}" ]; then
  ( eventually "eBPF service map has shop edges"          300 has_flows ) || { ebpf_diag; fail "eBPF flows required (E2E_REQUIRE_EBPF)"; }
  # kind nodes are containers sharing the runner's kernel: this is the
  # profiler's nested-PID-namespace path.
  ( eventually "eBPF CPU profiles for shop (nested PID namespaces)" 300 has_ebpf_cpu ) \
    || { kubectl -n "$NS" logs ds/kubehero-collector --tail=50 | grep -i profiler || true; fail "eBPF CPU profiles required (E2E_REQUIRE_EBPF)"; }
elif kubectl -n "$NS" logs ds/kubehero-collector > "$WORK/collector.log" 2>&1 && grep -q '"ebpf' "$WORK/collector.log"; then
  # Soft locally: some kind hosts lack cgroup_skb support. fail() exits,
  # so run it in a subshell to keep going either way.
  ( eventually "eBPF service map has shop edges"          300 has_flows ) \
    || { note "no eBPF flows yet (kernel without cgroup_skb?); continuing"; ebpf_diag; }
fi

# ─── 6. compatibility APIs (same tokens as the RPCs) ─────────────────────
MARK="e2e$(date +%s)"
NOW_NS="$(date +%s)000000000"
api -X POST "http://127.0.0.1:$PORT/loki/api/v1/push" -H 'Content-Type: application/json' -H 'X-Scope-OrgID: kind-e2e' \
  -d "$(jq -nc --arg ts "$NOW_NS" --arg line "loki push $MARK" '{streams: [{stream: {namespace: "e2e", app: "loki-push"}, values: [[$ts, $line]]}]}')" >/dev/null \
  && ok "Loki push accepted" || fail "Loki push"
api -X POST "http://127.0.0.1:$PORT/v1/logs" -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg ts "$NOW_NS" --arg line "otlp push $MARK" '{resourceLogs: [{resource: {attributes: [
        {key: "k8s.namespace.name", value: {stringValue: "e2e"}},
        {key: "k8s.cluster.name", value: {stringValue: "kind-e2e"}},
        {key: "service.name", value: {stringValue: "otlp-e2e"}}]},
      scopeLogs: [{logRecords: [{timeUnixNano: $ts, severityNumber: 17, body: {stringValue: $line}}]}]}]}')" >/dev/null \
  && ok "OTLP/HTTP logs accepted" || fail "OTLP push"
has_pushed()   { rpc LogsService/QueryLogs "$(jq -nc --arg q "{namespace=\"e2e\"} |= \"$MARK\"" '{query: $q, limit: 10}')" | jq -e '(.lines // []) | length >= 2'; }
has_loki()     { api -G "http://127.0.0.1:$PORT/loki/api/v1/query_range" --data-urlencode 'query={namespace="shop"}' --data-urlencode 'limit=5' | jq -e '.status == "success" and (.data.result | length) > 0'; }
has_opencost() { api "http://127.0.0.1:$PORT/allocation/compute?window=1h&aggregate=namespace" | jq -e '.code == 200 and (.data[0] | has("shop"))'; }
has_focus()    { api "http://127.0.0.1:$PORT/api/v1/export/focus?window=1d" -o "$WORK/focus.csv" && head -1 "$WORK/focus.csv" | grep -q BilledCost; }
eventually "pushed Loki + OTLP lines are queryable"       120 has_pushed
eventually "Loki query API returns shop streams"          60 has_loki
eventually "OpenCost allocation API lists shop"           60 has_opencost
eventually "FinOps FOCUS export"                          60 has_focus

# ─── 7. alerting ─────────────────────────────────────────────────────────
RULE_ID=$(rpc AlertsService/UpsertAlertRule "$(jq -nc '{rule: {name: "e2e: shop errors", kind: "logs",
  query: "sum(count_over_time({namespace=\"shop\"} |= \"request failed\" [5m]))", op: ">", threshold: 0,
  pendingFor: "0s", evalInterval: "15s", severity: "warn", enabled: true}}')" | jq -r '.rule.id // empty')
[ -n "$RULE_ID" ] && ok "alert rule created" || fail "UpsertAlertRule"
has_firing() { rpc AlertsService/ListAlerts '{"state":"firing"}' | jq -e --arg id "$RULE_ID" '[.alerts[]? | select(.ruleId == $id)] | length > 0'; }
eventually "the rule fires on the shop's errors"          240 has_firing

# ─── 8. rightsizing that adjusts (operator, guarded) ─────────────────────
CPU_BEFORE=$(checkout_cpu)
kubectl apply -f - >/dev/null <<EOF
apiVersion: kubehero.kubehero.io/v1
kind: RightsizingPolicy
metadata: { name: e2e-shop, namespace: $NS }
spec:
  scope:
    namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: shop } }
  mode: recommend
  humanArm: true
  minConfidence: low
  safety:
    minReplicas: 1
    observationWindow: "1h"
    maxChangePerDay: 1
EOF
rp() { kubectl -n "$NS" get rightsizingpolicy e2e-shop -o json; }
set_mode() { kubectl -n "$NS" patch rightsizingpolicy e2e-shop --type merge -p "{\"spec\":{\"mode\":\"$1\"}}" >/dev/null; }
has_recs()    { rp | jq -e '[.status.recommendations[]? | select(.workload == "checkout")] | length > 0'; }
has_planned() { rp | jq -e '[.status.plannedChanges[]? | select(.workload == "checkout" and .outcome == "planned")] | length > 0'; }
not_armed()   { rp | jq -e '.status.observedGeneration == .metadata.generation and ([.status.conditions[]? | select(.type == "Armed" and .status == "False")] | length > 0)'; }
applied()     { [ "$(checkout_cpu)" != "$CPU_BEFORE" ] && rp | jq -e '[.status.plannedChanges[]? | select(.workload == "checkout" and .outcome == "applied" and (.changeId // "") != "")] | length > 0'; }
eventually "recommend: the policy lists checkout"         300 has_recs
set_mode shadow
eventually "shadow: a checkout change is planned"         180 has_planned
[ "$(checkout_cpu)" = "$CPU_BEFORE" ] && ok "shadow changed nothing" || fail "shadow mode changed checkout"
set_mode apply
eventually "apply waits for a human to arm the policy"    120 not_armed
[ "$(checkout_cpu)" = "$CPU_BEFORE" ] && ok "unarmed apply changed nothing" || fail "unarmed apply changed checkout"
kubectl -n "$NS" annotate rightsizingpolicy e2e-shop kubehero.kubehero.io/armed=true --overwrite >/dev/null
eventually "armed apply rightsizes checkout"              240 applied
CHANGE_ID=$(rp | jq -r '[.status.plannedChanges[]? | select(.workload == "checkout" and .outcome == "applied")][0].changeId')
note "checkout cpu request $CPU_BEFORE → $(checkout_cpu) ($CHANGE_ID)"
kubectl -n shop get deploy checkout -o json | jq -e '.metadata.annotations["kubehero.io/rightsize-previous"] // "" | length > 0' >/dev/null \
  && ok "previous resources recorded for undo" || fail "no kubehero.io/rightsize-previous annotation"
kubectl -n "$NS" delete rightsizingpolicy e2e-shop --wait=true >/dev/null

# ─── 9. agents: advisor ──────────────────────────────────────────────────
pf kubehero-advisor "$APORT" 8083
arpc() { # arpc <Method> <json>: AdvisorService call as admin
  curl -sS --max-time 120 -X POST "http://127.0.0.1:$APORT/kubehero.v1.AdvisorService/$1" \
    -H "Content-Type: application/json" -H "Connect-Protocol-Version: 1" \
    -H "Authorization: Bearer $TOKEN" -d "$2"
}
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$APORT/kubehero.v1.AdvisorService/GetBriefing" \
  -H "Content-Type: application/json" -H "Connect-Protocol-Version: 1" -d '{"window":"1h"}')
[ "$code" = "401" ] && ok "advisor rejects anonymous callers" || fail "anonymous advisor call got $code"
has_brief()  { arpc GetBriefing '{"window":"1h"}' | jq -e '(.briefing.headline // "") | length > 0'; }
has_answer() { arpc Investigate '{"question":"why is checkout failing?","window":"1h"}' | jq -e '((.answerMarkdown // "") | length > 0) and ((.evidence // []) | length > 0)'; }
eventually "advisor briefing over live data"              120 has_brief
eventually "advisor investigation cites evidence"         180 has_answer

# ─── 10. CLI and MCP server ──────────────────────────────────────────────
if command -v go >/dev/null; then
  step "building the kubehero CLI"
  (cd "$ROOT/cli/kubehero" && GOWORK=off go build -o "$WORK/kubehero" .) || fail "CLI build"
  kh() { KUBEHERO_TOKEN="$TOKEN" "$WORK/kubehero" --endpoint "http://127.0.0.1:$PORT" \
    --advisor-endpoint "http://127.0.0.1:$APORT" --no-color "$@"; }
  # Output is captured before grep: grep -q on a pipe can SIGPIPE the CLI,
  # which pipefail would report as a failure.
  out=$(kh cost allocation --aggregate namespace --window 24h) && grep -q shop <<<"$out" \
    && ok "kubehero cost allocation" || fail "kubehero cost allocation"
  out=$(kh logs '{namespace="shop"} |= "request failed"' --since 15m --limit 5) && grep -q "request failed" <<<"$out" \
    && ok "kubehero logs" || fail "kubehero logs"
  out=$(kh rightsize --namespace shop --window 1h) && grep -q -E 'checkout|frontend|payments' <<<"$out" \
    && ok "kubehero rightsize" || fail "kubehero rightsize"
  kh alerts list >/dev/null && ok "kubehero alerts list" || fail "kubehero alerts list"
  [ -n "$(kh ask --quiet --window 1h 'why is checkout failing?')" ] && ok "kubehero ask" || fail "kubehero ask"
  # MCP over stdio: initialize, list the tools, call one. stdin stays open
  # a few seconds so the answers are written before the server sees EOF.
  mcp_out=$( ( printf '%s\n' \
      '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}' \
      '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
      '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
      '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_clusters","arguments":{}}}'; sleep 8 ) | kh mcp )
  echo "$mcp_out" | jq -se 'map(select(.id == 2))[0].result.tools | map(.name) | (index("get_cost_allocation") != null and index("query_logs") != null)' >/dev/null \
    && ok "kubehero mcp lists its tools" || fail "kubehero mcp tools/list: $(echo "$mcp_out" | head -c 400)"
  echo "$mcp_out" | jq -se 'map(select(.id == 3))[0].result | (.isError // false) == false' >/dev/null \
    && ok "kubehero mcp answers a tool call" || fail "kubehero mcp tools/call: $(echo "$mcp_out" | tail -c 400)"
  # Undo the operator's change with the CLI (kubeconfig + RBAC, like a human).
  kh undo "$CHANGE_ID" --namespace shop >/dev/null && ok "kubehero undo $CHANGE_ID" || fail "kubehero undo"
  [ "$(checkout_cpu)" = "$CPU_BEFORE" ] && ok "undo restored checkout ($CPU_BEFORE)" || fail "undo left checkout at $(checkout_cpu)"
else
  note "go not found: skipping the CLI, MCP and undo checks"
fi

# ─── 11. dashboard ───────────────────────────────────────────────────────
pf kubehero-dashboard "$DPORT" 3001
[ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$DPORT/api/healthz")" = "200" ] && ok "dashboard healthy" || fail "dashboard /api/healthz"
out=$(curl -fsS "http://127.0.0.1:$DPORT/login") && grep -qi 'access token' <<<"$out" && ok "dashboard offers token sign-in" || fail "dashboard /login"

FAILED=""
ok "end-to-end checks passed"
note "dashboard: kubectl -n $NS port-forward svc/kubehero-dashboard 3001:3001 (sign in with the admin token)"
