# KubeHero — reference Terraform (GKE)

Reference infrastructure for running KubeHero on Google Cloud: a regional
GKE cluster, Cloud SQL for PostgreSQL (private IP), ClickHouse in-cluster,
and the KubeHero Helm chart wired to all of it — in a single
`terraform apply`.

This is *reference* infrastructure: production-shaped defaults you are
expected to read and adapt, not a turnkey product. Everything is plain
`google_*` resources and two small local modules — no external module
registry dependencies.

## Layout

```
deploy/terraform/
├── README.md                     ← you are here
├── modules/
│   ├── gke-cluster/              # regional GKE, WI, DPv2, node pools + node SA
│   └── cloudsql-postgres/        # private-IP Cloud SQL, backups + PITR
└── gke/                          # root config — the thing you `terraform apply`
    ├── versions.tf               # pinned providers + commented GCS backend
    ├── providers.tf              # google / kubernetes / helm auth
    ├── network.tf                # VPC, subnets, Cloud NAT, Private Service Access
    ├── main.tf                   # module calls: cluster + node pools, Cloud SQL
    ├── secrets.tf                # random_password → Secret Manager + K8s secrets
    ├── clickhouse.tf             # helm_release: Bitnami ClickHouse on its own pool
    ├── kubehero.tf               # helm_release: oci://ghcr.io/kubehero-io/charts/kubehero
    ├── variables.tf / outputs.tf
    └── terraform.tfvars.example
```

What gets provisioned:

| Piece | Choice | Why |
|---|---|---|
| GKE | Regional, REGULAR channel, Workload Identity, Dataplane V2, private nodes + Cloud NAT | HA control plane; DPv2 makes the chart's `networkPolicies.enabled=true` enforceable; no SA key files anywhere |
| Node pools | `general` (on-demand, 3–6), `spot` (0–10), `clickhouse` (tainted, 1–3) | spot is the pool KubeHero meters savings on; ClickHouse never competes with app workloads |
| PostgreSQL | Cloud SQL, private IP only, TLS-only, PITR + 14 backups | control-plane metadata is low-volume but precious — let Google babysit it |
| ClickHouse | Bitnami chart as its **own** `helm_release`, single shard/replica, dedicated pool | see tradeoff below |
| KubeHero | `oci://ghcr.io/kubehero-io/charts/kubehero`, external-DB mode, production overlay values | matches `values.production.yaml` posture |

### ClickHouse: why not the chart's embedded subchart?

The kubehero chart *can* run ClickHouse as an embedded subchart
(`clickhouse.embedded=true`) — that is strictly less Terraform. We deliberately
don't use it here:

- the chart's embedded-storage path mints **dev-mode credentials** (its own
  `secrets.yaml` says production installs must not rely on it), and
  `values.production.yaml` ships `clickhouse.embedded: false`;
- embedding couples your time-series datastore's lifecycle to every app-chart
  upgrade/rollback — a routine `helm upgrade` shouldn't be able to touch the
  database;
- a dedicated, tainted node pool keeps ClickHouse's memory/IO appetite away
  from the control plane and lets you size it independently.

So this config runs the *same* Bitnami chart (same version the kubehero chart
vendors, see `deploy/helm/kubehero/Chart.lock`) as a separate release, and
points KubeHero at it via the standard `clickhouse.external.*` values. If you
outgrow one node, move to the Altinity operator — the chart values you'd touch
are the same `external.*` block.

## Prerequisites

- Terraform **>= 1.9** (config validated with 1.15)
- `gcloud` CLI, authenticated with credentials that can create the resources:

  ```sh
  gcloud auth application-default login
  ```

- A GCP project with billing enabled (required APIs are enabled by the config
  itself: container, sqladmin, servicenetworking, secretmanager, compute, iam)
- `kubectl` + `gke-gcloud-auth-plugin` for poking at the cluster afterwards
  (Terraform itself authenticates with your ADC token, not the plugin)
- Network egress from wherever you run Terraform to the cluster's public API
  endpoint (the Kubernetes/Helm providers talk to it during apply). Set
  `master_authorized_cidrs` to lock that endpoint to your ranges.

## Quickstart

```sh
cd deploy/terraform/gke
cp terraform.tfvars.example terraform.tfvars   # set project_id at minimum

terraform init -backend=false   # or configure the GCS backend first, below
terraform plan                  # review — ~40 resources
terraform apply
```

Apply takes ~20–30 minutes (GKE and Cloud SQL dominate). Then:

```sh
$(terraform output -raw kubeconfig_command)
kubectl -n kubehero get pods
$(terraform output -raw dashboard_hint)
```

## State backend (GCS)

Local state is fine for a first plan; anything shared or long-lived should use
a versioned GCS bucket. Create one, then uncomment the `backend "gcs"` block
in `gke/versions.tf`:

```sh
gcloud storage buckets create gs://YOUR-TF-STATE-BUCKET \
  --location=us --uniform-bucket-level-access
gcloud storage buckets update gs://YOUR-TF-STATE-BUCKET --versioning

terraform init -migrate-state
```

State contains the generated DB passwords (any Terraform state with
`random_password` does) — treat bucket access accordingly.

## Secrets

No secret is ever typed in or committed:

- `random_password` generates the PostgreSQL and ClickHouse passwords;
- a copy of each connection URL lands in **Secret Manager**
  (`<cluster>-postgres-url`, `<cluster>-clickhouse-url`) for operators;
- Kubernetes Secrets (`kubehero-postgres-credentials`,
  `kubehero-clickhouse-credentials`, key `url`) are what the chart's db-init
  job and control plane consume — the same contract as a manual install;
- ClickHouse's admin password reaches its chart via `set_sensitive`, so it
  never appears in plan output.

The chart's GCP cost adapter authenticates via **Workload Identity**
(`enable_gcp_cost_adapter`, on by default): a GSA with
`roles/monitoring.viewer` + `roles/compute.viewer`, bound to the KubeHero
service accounts. No exported keys.

## Cost expectations

Rough list-price math for `us-central1` with the defaults (July 2026 —
verify with the [pricing calculator](https://cloud.google.com/products/calculator)):

| Item | Sizing | ~$/month |
|---|---|---:|
| GKE management fee | 1 cluster | 73 |
| `general` pool | 3 × `e2-standard-4` on-demand | 295 |
| `clickhouse` pool | 1 × `e2-standard-4` + 100 GiB pd-ssd | 115 |
| `spot` pool | scales from 0; ~$29/node-month when used | 0+ |
| Cloud SQL | `db-custom-2-8192` ZONAL + 20 GiB SSD + backups | 110 |
| Cloud NAT | gateway + light egress | 35 |
| **Baseline total** | | **~630** |

Knobs that move it: `general_machine_type`/`general_min_nodes` down for dev
(`e2-standard-2` × 2 roughly halves the pool cost),
`postgres_availability_type = "REGIONAL"` (+~$100) and REGIONAL node pools
are the first things to turn *up* for production.

## Notes & deliberate choices

- **Prometheus/Grafana values are off** in the kubehero release: a fresh GKE
  cluster has no prometheus-operator CRDs, so emitting ServiceMonitors would
  fail the install. Install kube-prometheus-stack, then set
  `prometheus.enabled=true` / `grafana.enabled=true` in `kubehero.tf`.
- **No Ingress**: exposure is environment-specific (LB class, DNS, certs).
  Use the `dashboard_hint`/`api_hint` outputs, or enable the chart's
  `ingress.*` values once you have DNS + cert-manager sorted.
- **Deletion protection is on** for the cluster and Cloud SQL. To destroy:
  set `deletion_protection = false`, `terraform apply`, then
  `terraform destroy`. Cloud SQL instance names cannot be reused for ~7 days
  after deletion.
- **Single apply** works because the Kubernetes/Helm providers are configured
  from `module.gke` outputs. If you ever split ownership, apply infra first:
  `terraform apply -target=module.gke -target=module.postgres`.
- `.terraform.lock.hcl` is committed on purpose; run
  `terraform providers lock -platform=linux_amd64 -platform=darwin_arm64` if
  your team spans platforms.
