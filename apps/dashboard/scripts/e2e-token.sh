#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) KubeHero contributors
#
# Token-mode end-to-end check against a REAL control plane: sign-in via
# WhoAmI (no-JS form post), per-user token forwarding, role gating on a
# real admin RPC (ArmPolicy), and revocation → /api/auth/expired.
#
# The script owns the control plane: it generates throwaway API keys,
# starts CP_BIN with them (KUBEHERO_REQUIRE_AUTH=true), later restarts it
# without the viewer key, and stops it on exit. Start the dashboard
# (production build) first, pointed at CP_ADDR:
#
#   CONTROL_PLANE_URL=http://127.0.0.1:18080 KUBEHERO_DASHBOARD_AUTH=token \
#   KUBEHERO_SESSION_SECRET=$(openssl rand -hex 32) PORT=3002 \
#     node .next/standalone/apps/dashboard/server.js
#   CP_BIN=/path/to/control-plane scripts/e2e-token.sh
#
# Env: CP_BIN (required), CP_ADDR (127.0.0.1:18080), DASH (http://127.0.0.1:3002).
# Exits non-zero when any check fails. Keys never leave this process.
set -u
: "${CP_BIN:?set CP_BIN to a control-plane binary}"
CP_ADDR=${CP_ADDR:-127.0.0.1:18080}
B=${DASH:-http://127.0.0.1:3002}
HERE=$(cd "$(dirname "$0")" && pwd)
MANIFEST="$HERE/../.next/server/server-reference-manifest.json"
TMP=$(mktemp -d)
CP_PID=""
cleanup() { [ -n "$CP_PID" ] && kill "$CP_PID" 2>/dev/null; rm -rf "$TMP"; }
trap cleanup EXIT
ADMIN="kh_e2e_$(openssl rand -hex 16)"
VIEWER="kh_e2e_$(openssl rand -hex 16)"
ARM=$(node -e "const m=require('$MANIFEST').node;console.log(Object.keys(m).find(k=>m[k].exportedName==='setPolicyArmed'))")

pass=0; fail=0
ok() { pass=$((pass+1)); echo "ok    $1"; }
ko() { fail=$((fail+1)); echo "FAIL  $1"; }
expect() { if [ "$2" = "$3" ]; then ok "$1"; else ko "$1 (want '$2', got '$3')"; fi; }
contains() { if printf '%s' "$3" | grep -q -- "$2"; then ok "$1"; else ko "$1 (missing '$2')"; fi; }
start_cp() {
  [ -n "$CP_PID" ] && kill "$CP_PID" 2>/dev/null && wait "$CP_PID" 2>/dev/null
  KUBEHERO_API_KEYS="$1" KUBEHERO_REQUIRE_AUTH=true "$CP_BIN" serve --addr "$CP_ADDR" > "$TMP/cp.log" 2>&1 &
  CP_PID=$!
  for _ in $(seq 1 30); do curl -s -o /dev/null "http://$CP_ADDR/" && return; sleep 0.2; done
  echo "control plane did not start"; cat "$TMP/cp.log"; exit 2
}
# No-JS server-action sign-in: prints status/location/cookie flags, never the token.
login() { # token next jar
  local aid; aid=$(curl -s "$B/login" | grep -o 'name="\$ACTION_ID_[0-9a-f]*"' | head -1 | sed 's/name="//; s/"$//')
  curl -s -o /dev/null -D "$TMP/h" -c "$3" -X POST "$B/login" -H "Origin: $B" -F "$aid=" -F "next=$2" -F "token=$1"
  tr -d '\r' < "$TMP/h" | awk 'NR==1{print "status:",$2} tolower($1)=="location:"{print "location:",$2} tolower($1)=="set-cookie:"{split($2,a,"="); print "set-cookie:",a[1], ($0 ~ /HttpOnly/ ? "HttpOnly" : "NOT-HttpOnly"), ($0 ~ /SameSite=[Ll]ax/ ? "SameSite=Lax" : ""), ($0 ~ /Max-Age=[0-9]+/ ? "Max-Age" : "")}'
}
arm() { # jar armed
  curl -s -b "$1" -X POST "$B/budgets" -H "Next-Action: $ARM" -H "Origin: $B" -H "Accept: text/x-component" \
    -H "Content-Type: text/plain;charset=UTF-8" --data "[{\"policyName\":\"gpu-inference-cap\",\"armed\":$2,\"reason\":\"e2e\"}]"
}
code() { curl -s -o /dev/null -w '%{http_code}' --max-time 60 "$@"; }

start_cp "${ADMIN}:admin,${VIEWER}:viewer"
JA="$TMP/admin.jar"; JV="$TMP/viewer.jar"

expect "anon page → login" "307 $B/login?next=%2Foverview" "$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$B/overview")"
expect "/signup → login" "307 $B/login" "$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$B/signup")"
expect "/onboarding → login" "307 $B/login" "$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$B/onboarding")"
L=$(curl -s "$B/login")
contains "login asks for a token" 'name="token"' "$L"
printf '%s' "$L" | grep -q 'name="email"' && ko "no email sign-in in token mode" || ok "no email sign-in in token mode"
printf '%s' "$L" | grep -q 'without a token' && ko "no tokenless option while auth is required" || ok "no tokenless option while auth is required"

contains "bad token → invalid_token" "location: /login?error=invalid_token" "$(login kh_e2e_bogus /overview "$TMP/bogus.jar")"
R=$(login "$ADMIN" /overview "$JA")
contains "admin sign-in → /overview" "location: /overview" "$R"
contains "cookie HttpOnly, SameSite=Lax, Max-Age" "set-cookie: kh_session HttpOnly SameSite=Lax Max-Age" "$R"
expect "cookie Secure in production" "TRUE" "$(grep kh_session "$JA" | awk '{print $4}')"
expect "cookie holds no plaintext token" "0" "$(grep -c kh_e2e_ "$JA")"
contains "'Bearer ' prefix accepted" "location: /logs" "$(login "Bearer $VIEWER" /logs "$JV")"

N=$(curl -s -b "$JA" "$B/api/nav")
contains "nav: token mode" '"mode":"token"' "$N"
contains "nav: role from WhoAmI" '"role":"admin"' "$N"
contains "nav: control plane reachable with the user's token" '"health":"live"' "$N"
contains "nav: fleet read from the control plane" '"fleetSource":"live"' "$N"
contains "nav (viewer): role viewer" '"role":"viewer"' "$(curl -s -b "$JV" "$B/api/nav")"
for who in admin viewer; do
  jar=$JA; [ "$who" = viewer ] && jar=$JV
  bad=""
  for p in /overview /logs /profiles /network /fleet /allocation /chargeback /rightsizing /gpu /capacity /alerts /budgets /ceilings /advisor /ask /posture /settings /clusters/eks-use1-prod /workloads/eks-use1-prod/shop/cart; do
    c=$(code -b "$jar" "$B$p"); [ "$c" = 200 ] || bad="$bad $p:$c"
  done
  expect "every page renders for $who" "" "$bad"
done

contains "viewer: arm controls disabled" "requires the admin role" "$(curl -s -b "$JV" "$B/budgets")"
curl -s -b "$JA" "$B/budgets" | grep -q "requires the admin role" && ko "admin: arm controls enabled" || ok "admin: arm controls enabled"
contains "viewer: ArmPolicy → permission_denied" '"code":"permission_denied"' "$(arm "$JV" true)"
contains "admin: ArmPolicy → control-plane audit id" '"ok":true,"data":{"auditId":"' "$(arm "$JA" true)"
arm "$JA" false > /dev/null
expect "anon: server action → login" "307" "$(code -X POST "$B/budgets" -H "Next-Action: $ARM" -H "Origin: $B" -H 'Content-Type: text/plain;charset=UTF-8' --data '[{}]')"

start_cp "${ADMIN}:admin"   # revoke the viewer key
F=$(curl -s -b "$JV" "$B/fleet")
contains "revoked: page redirects to /api/auth/expired" "NEXT_REDIRECT;replace;/api/auth/expired" "$F"
contains "revoked: meta refresh for no-JS clients" 'http-equiv="refresh" content="1;url=/api/auth/expired"' "$F"
expect "revoked: /api/nav → /api/auth/expired" "307 /api/auth/expired" "$(curl -s -o /dev/null -D - -b "$JV" "$B/api/nav" | tr -d '\r' | awk 'NR==1{c=$2} tolower($1)=="location:"{l=$2} END{print c, l}')"
H=$(curl -s -o /dev/null -D - -b "$JV" "$B/api/auth/expired" | tr -d '\r')
contains "expired: cookie cleared" "set-cookie: kh_session=; Path=/; Max-Age=0" "$H"
contains "expired: → /login?reason=expired" "location: /login?reason=expired" "$H"
contains "login explains the expiry" "Your session expired or the token was revoked" "$(curl -s "$B/login?reason=expired")"
expect "admin unaffected" "200" "$(code -b "$JA" "$B/fleet")"
echo "PASS=$pass FAIL=$fail"
[ "$fail" -eq 0 ]
