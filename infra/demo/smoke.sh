#!/usr/bin/env bash
# KubeHero compose smoke / integration test.
#
# Boots the full stack via docker compose — including the demo generator,
# which streams a synthetic three-cluster fleet (with a live checkout
# retry-storm incident) through the REAL ingest APIs — then asserts that
# every signal comes back out of every public surface: Connect RPCs, the
# Loki / OpenCost / FOCUS compatibility endpoints, the advisor, the
# dashboard. Catches what unit tests miss: Dockerfiles, env wiring,
# migrations, ingest → rollups → queries, auth, start order.
#
#   ./infra/demo/smoke.sh         # run, tear down after
#   KEEP=1 ./infra/demo/smoke.sh  # leave the stack running on success

set -euo pipefail
cd "$(dirname "$0")/../.."

CYAN='\033[0;36m'; GREEN='\033[0;32m'; RED='\033[0;31m'; NC='\033[0m'
step() { printf "${CYAN}▸ %s${NC}\n" "$*"; }
ok()   { printf "${GREEN}✓ %s${NC}\n" "$*"; }
fail() { printf "${RED}✗ %s${NC}\n" "$*"; FAILED=1; exit 1; }

CP=http://127.0.0.1:8080
ADVISOR=http://127.0.0.1:8083

trap teardown EXIT
teardown() {
  if [[ -n "${KEEP:-}" || -n "${FAILED:-}" ]]; then
    printf "${CYAN}stack left running (docker compose logs <svc>; docker compose down -v to clean up)${NC}\n"
  else
    step "tearing down"; docker compose down -v >/dev/null 2>&1 || true
  fi
}

rpc() { # rpc <base> <Service/Method> <json>
  curl -sS --max-time 60 -X POST "$1/kubehero.v1.$2" \
    -H "Content-Type: application/json" -H "Connect-Protocol-Version: 1" -d "$3"
}
eventually() { # eventually <what> <seconds> <fn>
  local what="$1" limit="$2" fn="$3" deadline=$(( $(date +%s) + $2 ))
  until $fn >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || fail "$what (after ${limit}s)"
    sleep 5
  done
  ok "$what"
}

step "building images"
docker compose build >/dev/null 2>&1 || fail "compose build failed (re-run: docker compose build)"
ok "images built"

step "starting the stack"
docker compose up -d >/dev/null 2>&1 || fail "compose up failed"

cp_up() { curl -fs "$CP/healthz"; }
eventually "control plane healthy" 180 cp_up
[ "$(rpc $CP ControlPlaneService/HealthCheck '{}' | jq -r .status)" = "ok" ] && ok "HealthCheck" || fail "HealthCheck"

# The generator backfills 72h at 5-minute resolution before going live.
backfilled() { docker compose logs demo-generator 2>/dev/null | grep -q "backfill complete"; }
eventually "demo generator backfilled 72h of history" 900 backfilled

has_alloc()     { rpc $CP CostService/GetAllocation '{"window":"24h","aggregate":["namespace"]}' | jq -e '.source != "demo" and ([.allocations[] | select(.name=="checkout")] | length == 1) and .totals.totalCost > 0'; }
has_idle()      { rpc $CP CostService/GetAllocation '{"window":"24h","aggregate":["cluster"],"includeIdle":true}' | jq -e '[.allocations[] | select(.name | test("idle"))] | length > 0'; }
has_logs()      { rpc $CP LogsService/QueryLogs '{"query":"{namespace=\"checkout\"} |= \"retrying\"","limit":5}' | jq -e '(.lines|length) > 0'; }
has_metric()    { rpc $CP LogsService/QueryLogs '{"query":"sum by (level) (count_over_time({namespace=\"checkout\"}[5m]))"}' | jq -e '.resultType == "matrix" and (.series|length) > 0'; }
has_patterns()  { rpc $CP LogsService/GetLogPatterns '{"query":"{namespace=\"checkout\"}"}' | jq -e '[.patterns[] | select(.pattern | test("payment gateway timeout"))] | length > 0'; }
has_rightsize() { rpc $CP CostService/ListRightsizing '{"window":"3d"}' | jq -e '[.recommendations[] | select(.workload=="frontend-gateway" and .direction=="downsize")] | length > 0'; }
has_oom_guard() { rpc $CP CostService/ListRightsizing '{"window":"3d"}' | jq -e '[.recommendations[] | select(.workload=="payments-worker")] | .[0].direction != "downsize" and .[0].oomKills > 0'; }
has_flame()     { rpc $CP ProfilesService/GetFlamegraph '{"selector":{"service":"checkout-api"}}' | jq -e '[.nodes[] | select(.name | test("crypto/tls"))] | length > 0'; }
has_top()       { rpc $CP ProfilesService/GetTopFunctions '{"selector":{"service":"checkout-api"},"limit":5}' | jq -e '(.functions|length) > 0'; }
has_map()       { rpc $CP NetworkService/GetServiceMap '{"clusterId":"eks-use1-prod"}' | jq -e '.source != "demo" and (.edges|length) > 0 and ([.edges[] | select(.egress)] | length > 0)'; }
has_anomaly()   { rpc $CP ControlPlaneService/ListAnomalies '{"window":"24h"}' | jq -e '[.anomalies[] | select(.subject | test("checkout"))] | length > 0'; }
has_loki()      { curl -fsG "$CP/loki/api/v1/query_range" --data-urlencode 'query={namespace="checkout"}' --data-urlencode 'limit=5' | jq -e '.status == "success" and (.data.result|length) > 0'; }
has_opencost()  { curl -fs "$CP/allocation/compute?window=1d&aggregate=namespace" | jq -e '.code == 200 and (.data[0]|length) > 0'; }
has_focus()     { curl -fs "$CP/api/v1/export/focus?window=1d" | head -1 | grep -q BilledCost; }
has_briefing()  { rpc $ADVISOR AdvisorService/GetBriefing '{"window":"24h"}' | jq -e '.briefing.headline | length > 0'; }
has_answer()    { rpc $ADVISOR AdvisorService/Investigate '{"question":"why did checkout spend jump?","window":"24h"}' | jq -e '(.answerMarkdown | test("checkout"; "i")) and (.evidence|length) > 0'; }
has_ui()        { curl -fs -o /dev/null http://127.0.0.1:3001/login; }

eventually "allocation by namespace (real data)"           120 has_alloc
eventually "idle cost rows"                                 60 has_idle
eventually "LogQL line filter"                              60 has_logs
eventually "LogQL metric query"                             60 has_metric
eventually "Drain patterns find the retry storm"            60 has_patterns
eventually "rightsizing downsizes the idle gateway"        120 has_rightsize
eventually "rightsizing never shrinks the OOM-ing worker"   60 has_oom_guard
eventually "flamegraph shows the TLS handshake regression"  60 has_flame
eventually "top functions"                                  60 has_top
eventually "eBPF-style service map with egress edges"       60 has_map
eventually "spend anomaly on checkout"                     120 has_anomaly
eventually "Loki query API"                                 60 has_loki
eventually "OpenCost allocation API"                        60 has_opencost
eventually "FinOps FOCUS export"                            60 has_focus
eventually "advisor briefing"                               90 has_briefing
eventually "advisor investigation cites evidence"          120 has_answer
eventually "dashboard serves /login"                        90 has_ui

ok "compose smoke passed"
