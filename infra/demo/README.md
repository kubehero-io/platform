# kind demo — KubeHero end-to-end

Two flavors. Pick the one that matches what you want to see.

## `task demo` — full stack + policy that trips

The flagship demo. One command: kind cluster, all 5 KubeHero services, demo workloads, and a `BudgetPolicy` + `CeilingPolicy` sized to trip within ~3 minutes of the collector first ingesting.

```bash
task demo                              # ~5-7 min cold
task demo:clean                        # tear down
# or:
./infra/demo/task-demo.sh
./infra/demo/task-demo.sh --clean
```

When it's ready you can:

- port-forward the dashboard at `:3001` and log in
- watch `kubectl describe ceilingpolicy ml-inference-ceiling` — burn rate updates each reconcile
- query the audit log via the cp's `ListAuditLog` RPC and see the `ceiling.tripped` event land

The tripping policy lives in `tripping-policy.yaml`. It's `hardStop=false` by default (alert-only); flip it to `true` and arm the policy to see the escalator actually cap HPAs.

## `kind-demo.sh` — collector + Prometheus only

The original, minimal flavor. Boots Grafana with three pre-loaded chargeback dashboards. Useful when you only want to look at the metric shape and don't need the policy / control-plane story.

```bash
./infra/demo/kind-demo.sh              # full setup
./infra/demo/kind-demo.sh --clean      # tear down
```

After it finishes (~4 minutes):

- **Grafana** at http://localhost:30080 (admin / kubehero) with three pre-loaded dashboards in the **KubeHero** folder
- **Prometheus** scraping the collector and applying the chargeback recording rules
- **Demo workloads** across `ml-inference`, `retrieval`, `data`, `edge` namespaces

## Prerequisites

- `docker` (any recent version)
- `kind` 0.20+
- `helm` 3.12+
- `kubectl`
- `task` (for `task demo`) — `brew install go-task/tap/go-task`
