#!/usr/bin/env bash
# What the e2e cluster looked like when a check failed: nodes, pods,
# events, every KubeHero workload's logs, the operator's policies and the
# shop. Best effort: every command may fail on a half-built cluster.
set -u
NS=kubehero-system

group() { echo "::group::$*"; }
end() { echo "::endgroup::"; }

kubectl get nodes -o wide || true
kubectl get pods -A -o wide || true
group "events ($NS)"; kubectl -n "$NS" get events --sort-by=.lastTimestamp | tail -100 || true; end
for w in $(kubectl -n "$NS" get deploy,statefulset,daemonset -o name 2>/dev/null); do
  group "logs $w"; kubectl -n "$NS" logs "$w" --all-containers --tail=300 || true; end
done
group "policies"
kubectl get rightsizingpolicies,budgetpolicies,ceilingpolicies -A -o yaml 2>/dev/null | head -300 || true
end
group "shop"
kubectl -n shop get pods -o wide || true
kubectl -n shop get deploy checkout -o yaml 2>/dev/null | head -80 || true
end
