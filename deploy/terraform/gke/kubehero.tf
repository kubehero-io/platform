resource "kubernetes_namespace_v1" "kubehero" {
  metadata {
    name = "kubehero"

    labels = {
      "app.kubernetes.io/managed-by" = "terraform"
      # Restricted Pod Security admission — the chart's default security
      # contexts are built to pass it.
      "pod-security.kubernetes.io/enforce" = "restricted"
    }
  }

  depends_on = [module.gke]
}

# ─── Workload Identity for the GCP cost adapter ─────────────────────────────
# The control plane exchanges its KSA token for this GSA — no key files.

resource "google_service_account" "kubehero" {
  count = var.enable_gcp_cost_adapter ? 1 : 0

  project      = var.project_id
  account_id   = "${var.cluster_name}-cp"
  display_name = "KubeHero control plane (${var.cluster_name})"
}

resource "google_project_iam_member" "kubehero" {
  for_each = var.enable_gcp_cost_adapter ? toset([
    "roles/monitoring.viewer", # utilization metrics
    "roles/compute.viewer",    # machine types, disks, committed-use info
  ]) : toset([])

  project = var.project_id
  role    = each.value
  member  = google_service_account.kubehero[0].member
}

# The chart stamps serviceAccount.annotations on the control-plane,
# operator and pricing-engine KSAs (release fullname prefix), so bind
# all three to the GSA.
resource "google_service_account_iam_member" "workload_identity" {
  for_each = var.enable_gcp_cost_adapter ? toset([
    "control-plane",
    "operator",
    "pricing-engine",
  ]) : toset([])

  service_account_id = google_service_account.kubehero[0].name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${module.gke.workload_pool}[${kubernetes_namespace_v1.kubehero.metadata[0].name}/kubehero-${each.value}]"
}

# ─── KubeHero ───────────────────────────────────────────────────────────────
# Every key below exists in deploy/helm/kubehero/values.yaml; the external
# DB contract (existingSecret with key `url`) matches templates/db-init-job.

locals {
  kubehero_values = {
    cluster = {
      id = var.cluster_name
    }

    # Production posture (mirrors values.production.yaml).
    priorityClass       = { create = true }
    podDisruptionBudget = { enabled = true }
    # Enforced by Dataplane V2 (datapath_provider = ADVANCED_DATAPATH).
    networkPolicies = { enabled = true }

    # A fresh GKE cluster has no prometheus-operator CRDs, so emitting
    # ServiceMonitor/PrometheusRule objects would fail the install.
    # Install kube-prometheus-stack first, then flip these on.
    prometheus = { enabled = false }
    grafana    = { enabled = false }

    postgresql = {
      embedded = false
      external = {
        host           = module.postgres.private_ip
        port           = 5432
        database       = module.postgres.database
        sslMode        = "require"
        existingSecret = kubernetes_secret_v1.postgres.metadata[0].name
      }
    }

    clickhouse = {
      embedded = false
      external = {
        host           = local.clickhouse_host
        port           = 9000 # native TCP on the Bitnami service
        database       = "kubehero"
        existingSecret = kubernetes_secret_v1.clickhouse.metadata[0].name
      }
    }

    serviceAccount = var.enable_gcp_cost_adapter ? {
      annotations = {
        "iam.gke.io/gcp-service-account" = google_service_account.kubehero[0].email
      }
    } : {}

    cloud = {
      gcp = {
        enabled        = var.enable_gcp_cost_adapter
        serviceAccount = var.enable_gcp_cost_adapter ? google_service_account.kubehero[0].email : ""
        projects       = [var.project_id]
      }
    }
  }
}

resource "helm_release" "kubehero" {
  name      = "kubehero"
  namespace = kubernetes_namespace_v1.kubehero.metadata[0].name

  repository = "oci://ghcr.io/kubehero-io/charts"
  chart      = "kubehero"
  version    = var.kubehero_chart_version

  wait    = true
  timeout = 600

  values = [yamlencode(local.kubehero_values)]

  depends_on = [
    helm_release.clickhouse,       # CLICKHOUSE_URL must resolve for the db-init job
    kubernetes_secret_v1.postgres, # chart reads `url` from these
    kubernetes_secret_v1.clickhouse,
    google_service_account_iam_member.workload_identity,
  ]
}
