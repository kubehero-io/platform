# kubehero-platform

Monorepo for **KubeHero** — open-source, self-hosted Kubernetes cost monitoring across AKS, GKE, and EKS. Find idle CPU, forgotten namespaces, and underused GPUs, then enforce a hard spending ceiling with Kubernetes-native policy CRDs.

## Layout

```
apps/        dashboard · docs
services/    control-plane · collector · pricing-engine · operator
cli/         kubehero (Go + cobra)
packages/    proto · ui · cost-model · tsconfig
deploy/      helm · terraform
infra/       dagger
```

## Licensing

Open source. See `ARCHITECTURE.md` §1 for the exact split.

- Apache 2.0 — agent, CLI, collector, cost-model, proto
- BSL 1.1 — control-plane, operator, pricing-engine, dashboard (source-available, self-hostable, free to run)

## Getting started

```
task build       # builds every Go module + TS/JS workspace
task test        # go test ./...
task helm:lint   # lints the chart
task dev:dashboard
task dev:control-plane
```

See `Taskfile.yml` for the full list.

## Contributing

See `CONTRIBUTING.md`.
