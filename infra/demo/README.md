# Demos and end-to-end tests

Three ways to see KubeHero run for real. All of them exercise the real
images, the real ingest path and the real stores — nothing is mocked.

## `docker compose` — full stack, no cluster (fastest)

```bash
docker compose up --build -d      # from the repo root
open http://localhost:3001         # dashboard (demo sign-in)
```

The `demo-generator` service streams a synthetic three-cluster fleet
(`eks-use1-prod`, `gke-euw4-batch`, `aks-westeu-prod-01`) through the
real ingest APIs: pod + node cost, container usage, logs, CPU profiles,
eBPF-style flows and cluster events, with 72h of backfilled history. The
stories are consistent across every signal:

- **checkout retry storm** (began 3h before start): spend anomaly, a
  flood of `payment gateway timeout, retrying` logs, TLS handshakes in
  the CPU profile, egress + retransmits to `api.stripe.com`
- **payments-worker OOMs** every ~2h — rightsizing must never shrink it
- **idle A100s** on `model-server-a100` (~22% GPU utilisation)
- **over-provisioned gateways** — clear downsizing recommendations
- **etl-backfill can't schedule** — capacity demand with a priced fix

`./infra/demo/smoke.sh` boots the stack and asserts all of the above
through every public API (Connect, Loki, OpenCost, FOCUS, advisor,
dashboard). `task smoke` runs it.

## `e2e-kind.sh` — the real chart on a local cluster

```bash
./infra/demo/e2e-kind.sh              # build, install, assert, tear down
KEEP=1 ./infra/demo/e2e-kind.sh       # …and keep the cluster
```

Builds every image, loads them into kind, installs the Helm chart with
embedded Postgres + ClickHouse and auth on, runs a small live "shop"
(three real services built from the generator's `workload` mode: HTTP
traffic, JSON logs, CPU, `/debug/pprof`) and waits until the real
collector's data answers every query: allocation, LogQL, rightsizing,
profiles and — when the kernel allows eBPF — the service map.

## `task-demo.sh` — the guided demo

```bash
task demo            # = ./infra/demo/task-demo.sh
task demo:clean
```

`e2e-kind.sh` plus the classic demo workloads and a BudgetPolicy +
CeilingPolicy sized to trip (`tripping-policy.yaml`), then prints the
dashboard / CLI commands and your admin token.

## Prerequisites

docker, kind (≥ 0.30), helm (≥ 3.15), kubectl, curl, jq. Docker Desktop
needs ~6 GB of memory for the kind flavours.
