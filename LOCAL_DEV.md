# Local development

Four supported modes. Pick the one that matches what you're building.

## 1. Docker Compose (no Kubernetes)

Fastest path — the full stack in a couple of minutes, with a synthetic
three-cluster fleet streamed through the real ingest APIs.

```bash
docker compose up --build -d

# Dashboard       http://localhost:3001  (demo sign-in)
# Control plane   http://localhost:8080  (Connect-RPC + Loki / OpenCost / OTLP / Pyroscope APIs)
# Advisor         http://localhost:8083  (set ANTHROPIC_API_KEY for the Claude brain)
# Grafana         http://localhost:3000  (admin / kubehero) — Prometheus + "KubeHero Logs" (Loki) datasources
# Prometheus      http://localhost:9090
# ClickHouse      http://localhost:8123  (kubehero / kubehero)
```

Use it when you're hacking on the control plane, advisor or dashboard.
`./infra/demo/smoke.sh` asserts every signal end to end.
Tear down: `docker compose down -v`.

## 2. Services on the host

```bash
docker compose up -d postgres clickhouse
export DATABASE_URL=postgres://kubehero:kubehero@localhost:5432/kubehero?sslmode=disable
export CLICKHOUSE_URL=clickhouse://kubehero:kubehero@localhost:9000/kubehero
go run ./services/control-plane serve                     # :8080
go run ./infra/demo/generator --backfill 24h              # fleet data
CONTROL_PLANE_URL=http://localhost:8080 go run ./services/advisor serve
pnpm --filter @kubehero/dashboard dev                     # :3001 (reads .env.local)
```

Integration tests run against the same containers:

```bash
KUBEHERO_TEST_CLICKHOUSE_URL=clickhouse://kubehero:kubehero@localhost:9000/kubehero \
  go test -tags integration ./services/control-plane/...
```

## 3. kind (real cluster, real collector)

```bash
./infra/demo/e2e-kind.sh          # build + install the chart + assert every signal
KEEP=1 ./infra/demo/e2e-kind.sh   # keep the cluster afterwards
task demo                         # the guided demo (adds a policy that trips)
```

The collector's eBPF programs run inside kind too (they share Docker
Desktop's Linux kernel); the collector logs one line and carries on if
the kernel doesn't allow them.

## 4. Tilt (inner loop on Kubernetes)

Needs a local cluster (kind / minikube / docker-desktop / k3d) + Tilt.

```bash
kind create cluster --name kubehero
tilt up
```

The `Tiltfile` rebuilds images on change and helm-installs the chart
into `kubehero-system`.

## Sample policies

Everything in `config/samples/` is a ready-to-apply CRD:

```bash
kubectl apply -f config/samples/rightsizing-dev.yaml
kubehero cap --arm --policy prod-monthly-ceiling
```

CRD reference: https://kubehero.io/docs/crd-reference
