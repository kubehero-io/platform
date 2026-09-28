#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) KubeHero contributors
#
# Route smoke test for a running dashboard in DEMO mode (no
# CONTROL_PLANE_URL): every page, the not-found and redirect paths, the
# auth gate, and every API route (JSON, CSV, both SSE streams).
#
#   KUBEHERO_SESSION_SECRET=<server's secret> scripts/smoke-routes.sh [http://127.0.0.1:3001]
#
# Exits non-zero when any check fails.
set -u
BASE=${1:-http://127.0.0.1:3001}
HERE=$(cd "$(dirname "$0")" && pwd)
CK=$(node "$HERE/mint-session.mjs")
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
pass=0; fail=0
check() { # name want got
  if [ "$2" = "$3" ]; then pass=$((pass+1)); printf "ok    %-4s %s\n" "$3" "$1"; else fail=$((fail+1)); printf "FAIL  %-4s %s (want %s)\n" "$3" "$1" "$2"; fi
}
page() { # path [want]
  local want=${2:-200} got
  got=$(curl -s -o "$TMP/body" -w "%{http_code}" --max-time 60 -b "kh_session=$CK" "$BASE$1")
  if [ "$got" = "200" ] && grep -q -E "Application error|Internal Server Error|Unhandled Runtime Error" "$TMP/body"; then
    fail=$((fail+1)); printf "FAIL  %-4s %s (error marker in body)\n" "$got" "$1"; return
  fi
  check "$1" "$want" "$got"
}
# Unknown ids render the segment's not-found UI. Those routes stream
# behind loading.tsx, so Next answers 200 + <meta name="robots" content="noindex">.
notfound() { # path text
  local got; got=$(curl -s -o "$TMP/nf" -w "%{http_code}" --max-time 60 -b "kh_session=$CK" "$BASE$1")
  if grep -q "$2" "$TMP/nf" && grep -q noindex "$TMP/nf"; then check "$1 (not-found UI, noindex)" "$got" "$got"; else check "$1 (not-found UI)" "not-found" "$got"; fi
}
location() { # curl args… → Location header value
  curl -s -o /dev/null -D - "$@" | tr -d '\r' | awk 'tolower($1)=="location:"{print $2}'
}

for p in /overview /logs /profiles /network /fleet /allocation /chargeback /rightsizing /gpu /capacity /alerts /budgets /ceilings /advisor /ask /posture /settings \
  "/logs?q=%7Bnamespace%3D%22shop%22%7D%20%7C%3D%20%22error%22&window=1h" \
  "/logs?q=sum%20by%20(level)%20(count_over_time(%7Bnamespace%3D%22shop%22%7D%5B5m%5D))&window=6h" \
  "/logs?tab=patterns&q=%7Bnamespace%3D%22shop%22%7D" "/logs?live=1" \
  "/allocation?agg=workload&window=7d&idle=weighted" "/allocation?agg=label%3Ateam&window=30d" \
  "/rightsizing?conf=high&direction=downsize" "/alerts?tab=rules" "/alerts?tab=silences" "/alerts?tab=rules&rule=new" \
  "/profiles?type=alloc_space" "/network?cluster=eks-use1-prod" "/ask?q=why%20did%20cost%20spike" \
  /clusters/eks-use1-prod /workloads/eks-use1-prod/shop/cart; do page "$p"; done
notfound /clusters/does-not-exist "Cluster not found"
notfound /workloads/bad%20name/x/y "Workload not found"
page /no-such-page 404

check "/waste → /rightsizing (keeps q)" "/rightsizing?q=cart" "$(location -b "kh_session=$CK" "$BASE/waste?q=cart")"
check "/ (signed in) → /overview" "/overview" "$(location -b "kh_session=$CK" "$BASE/")"
check "anon page → login" "/login?next=%2Foverview" "$(location "$BASE/overview")"
check "gate Location ignores a foreign Host" "/login?next=%2Flogs" "$(location -H "Host: dash.example.com" "$BASE/logs")"
check "/login anon" "200" "$(curl -s -o /dev/null -w "%{http_code}" "$BASE/login")"
for a in /api/nav "/api/logs/tail?q=%7Bnamespace%3D%22shop%22%7D" "/api/export/allocation?window=7d" "/api/export/focus?window=30d"; do
  check "$a anon" "401" "$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "$BASE$a")"
done
check "/api/ask anon" "401" "$(curl -s -o /dev/null -w "%{http_code}" -X POST -H 'content-type: application/json' -d '{"question":"x"}' "$BASE/api/ask")"

check "/api/nav" "200" "$(curl -s -o "$TMP/nav" -w "%{http_code}" -b "kh_session=$CK" "$BASE/api/nav")"
check "/api/export/allocation (csv)" "200" "$(curl -s -o "$TMP/csv" -w "%{http_code}" -b "kh_session=$CK" "$BASE/api/export/allocation?window=7d&aggregate=namespace")"
check "allocation csv has a source column" "source" "$(head -1 "$TMP/csv" | tr -d '\r' | tr ',' '\n' | tail -1)"
check "/api/export/focus (csv)" "200" "$(curl -s -o "$TMP/focus" -w "%{http_code}" -b "kh_session=$CK" "$BASE/api/export/focus?window=30d&aggregate=workload")"
curl -s -N --max-time 4 -b "kh_session=$CK" "$BASE/api/logs/tail?q=%7Bnamespace%3D%22shop%22%7D" > "$TMP/tail" 2>/dev/null
n=$(grep -c "^event: lines" "$TMP/tail"); [ "$n" -ge 1 ] && check "/api/logs/tail SSE ($n line events in 4s)" x x || check "/api/logs/tail SSE" events none
curl -s -N --max-time 20 -b "kh_session=$CK" -X POST -H 'content-type: application/json' -H 'accept: text/event-stream' \
  -d '{"question":"why did the shop namespace cost spike yesterday?","window":"24h"}' "$BASE/api/ask" > "$TMP/ask" 2>/dev/null
grep -q "^event: result" "$TMP/ask" && check "/api/ask SSE ends with a result" x x || check "/api/ask SSE" result none
check "/api/logs/tail rejects a bad query" "400" "$(curl -s -o /dev/null -w "%{http_code}" -b "kh_session=$CK" "$BASE/api/logs/tail?q=%7Bbroken")"
check "/api/auth/expired → login (host-independent)" "/login?reason=expired" "$(location -H "Host: dash.example.com" "$BASE/api/auth/expired")"
echo "PASS=$pass FAIL=$fail"
[ "$fail" -eq 0 ]
