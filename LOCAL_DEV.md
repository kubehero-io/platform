# Local development

Three supported modes. Pick the one that matches what you're building.

## 1. Docker Compose (no K8s needed)

Fastest path — spins up the full stack in ~90 seconds.

```bash
docker compose up --build -d

# Dashboard at     http://localhost:3001
# Control plane at http://localhost:8080
# Grafana at       http://localhost:3000  (admin / kubehero)
# Prometheus at    http://localhost:9090
```

Use when you're hacking on a Go service or the dashboard and don't
need real K8s semantics (no CRDs, no RBAC, no operator reconcile loop).

Tear down: `docker compose down -v`

## 2. Tilt (K8s inner loop)

Needs: local K8s (kind / minikube / docker-desktop / k3d) + Tilt.

```bash
kind create cluster --name kubehero
tilt up
```

`Tiltfile` in the repo root watches your sources, rebuilds images
on change, and helm-installs the chart into `kubehero-system`. Ports
are forwarded automatically.

Use this when you need CRD reconciliation or chart-level changes.

## 3. Kind end-to-end demo

One-command full-stack with kube-prometheus-stack + demo workloads:

```bash
./infra/demo/kind-demo.sh
```

Details in `infra/demo/README.md`. Use this to demo the chargeback
dashboards against synthetic team workloads.

## Which sample policies to try

Everything in `config/samples/` is a ready-to-apply CRD:

```bash
kubectl apply -f config/samples/
kubehero cap --arm --policy prod-monthly-ceiling
```

See the docs at `apps/docs/content/docs/crd-reference.mdx` or
https://kubehero.io/docs/crd-reference.
