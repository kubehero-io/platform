#!/usr/bin/env bash
# KubeHero compose smoke test.
#
# Boots the full stack via docker compose, runs Connect-RPC roundtrips
# against the live control-plane, verifies the dashboard serves /login,
# and tears down. Catches whole classes of breakage that `go test`
# misses: bad Dockerfiles, env-var wiring, container start order,
# port conflicts, image runtime issues.
#
#   ./infra/demo/smoke.sh         # run the full test
#   KEEP=1 ./infra/demo/smoke.sh  # leave the stack running on success

set -euo pipefail

cd "$(dirname "$0")/../.."

CYAN='\033[0;36m'
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

step() { printf "${CYAN}▸ %s${NC}\n" "$*"; }
ok()   { printf "${GREEN}✓ %s${NC}\n" "$*"; }
fail() { printf "${RED}✗ %s${NC}\n" "$*"; exit 1; }

trap teardown EXIT
teardown() {
  if [[ -z "${KEEP:-}" ]]; then
    step "tearing down compose stack"
    docker compose down -v >/dev/null 2>&1 || true
  else
    printf "${CYAN}stack still running; \`docker compose down -v\` to clean up${NC}\n"
  fi
}

step "building all 6 images"
docker compose build collector control-plane pricing-engine dashboard \
  >/dev/null 2>&1 || fail "compose build failed (re-run without redirect for log)"
ok "images built"

step "starting stack"
docker compose up -d \
  postgres clickhouse valkey collector control-plane pricing-engine dashboard \
  >/dev/null 2>&1 || fail "compose up failed"

step "waiting for postgres + clickhouse healthchecks"
for i in {1..60}; do
  if docker compose ps --format json 2>/dev/null | grep -q '"Health":"healthy"'; then
    ok "storage healthy"
    break
  fi
  sleep 2
  if [[ $i -eq 60 ]]; then
    fail "storage didn't become healthy in 120s"
  fi
done

step "waiting for /healthz on control-plane"
for i in {1..30}; do
  if curl -fs http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
    ok "control-plane up"
    break
  fi
  sleep 2
  if [[ $i -eq 30 ]]; then
    fail "control-plane didn't respond on /healthz in 60s"
  fi
done

step "Connect-RPC smoke: HealthCheck"
res=$(curl -fs -X POST http://127.0.0.1:8080/kubehero.v1.ControlPlaneService/HealthCheck \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' -d '{}')
[[ "$res" == *'"status":"ok"'* ]] || fail "HealthCheck didn't return status:ok ($res)"
ok "HealthCheck → $res"

step "Connect-RPC smoke: RegisterCluster"
reg=$(curl -fs -X POST http://127.0.0.1:8080/kubehero.v1.ControlPlaneService/RegisterCluster \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  -d '{"name":"smoke-test-cluster","cloud":"aws","region":"us-east-1"}')
[[ "$reg" == *'"token":"'* ]] || fail "RegisterCluster didn't include a token ($reg)"
[[ "$reg" == *'"helmInstall":"helm install kubehero kubehero/kubehero'* ]] \
  || fail "RegisterCluster helm snippet shape changed ($reg)"
ok "RegisterCluster → token + helm snippet"

step "Connect-RPC smoke: AppendAuditEntry"
res=$(curl -fs -X POST http://127.0.0.1:8080/kubehero.v1.ControlPlaneService/AppendAuditEntry \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  -d '{"action":"smoke.test","targetKind":"SmokeTest","targetName":"compose","outcome":"applied"}')
[[ "$res" == *'"id":'* ]] || fail "AppendAuditEntry didn't return an id ($res)"
ok "AppendAuditEntry → $res"

step "Connect-RPC smoke: ListAuditLog (round-trip)"
list=$(curl -fs -X POST http://127.0.0.1:8080/kubehero.v1.ControlPlaneService/ListAuditLog \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' -d '{"limit":50}')
[[ "$list" == *'"action":"smoke.test"'* ]] \
  || fail "smoke event not visible via ListAuditLog ($list)"
ok "ListAuditLog round-trip OK"

step "Connect-RPC smoke: IngestPodCost (cp → ClickHouse)"
ing=$(curl -fs -X POST http://127.0.0.1:8080/kubehero.v1.ControlPlaneService/IngestPodCost \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  -d '{"clusterId":"smoke-cluster","samples":[
    {"pod":"smoke-pod-a","namespace":"smoke","team":"smoke","cluster":"smoke-cluster","cpuMillicores":500,"memBytes":1073741824,"costUsdSec":0.0001},
    {"pod":"smoke-pod-b","namespace":"smoke","team":"smoke","cluster":"smoke-cluster","cpuMillicores":1000,"memBytes":2147483648,"costUsdSec":0.0002}
  ]}')
[[ "$ing" == *'"written":2'* ]] || fail "IngestPodCost didn't write 2 rows ($ing)"
ok "IngestPodCost wrote 2 rows"

step "ClickHouse round-trip: SELECT the rows we just wrote"
# Tiny grace for ClickHouse to flush — MergeTree commits are fast but
# not synchronous with the cp's INSERT ack.
sleep 1
ch=$(curl -fs "http://127.0.0.1:8123/?database=kubehero" \
  --user 'kubehero:kubehero' \
  --data-binary "SELECT count() FROM pod_cost_1s WHERE cluster_id='smoke-cluster'")
ch_count=$(echo "$ch" | tr -d '[:space:]')
[[ "$ch_count" == "2" ]] || fail "ClickHouse SELECT returned '$ch_count' rows (expected 2)"
ok "ClickHouse has the rows"

step "dashboard /login responds 200"
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:3001/login)
[[ "$code" == "200" ]] || fail "dashboard /login returned $code"
ok "dashboard /login 200"

printf "\n${GREEN}smoke test passed.${NC}\n"
