# Credentials are generated here, never typed in and never committed.
#   · random_password         → the only place a password is minted
#   · Secret Manager          → durable copy for operators / break-glass
#   · kubernetes_secret       → what the kubehero chart actually consumes
#     (the chart's db-init job + control plane read key `url` from the
#     secret named by <db>.external.existingSecret)

resource "random_password" "postgres" {
  length  = 32
  special = false # keeps the password URL-safe without escaping
}

resource "random_password" "clickhouse" {
  length  = 32
  special = false
}

locals {
  clickhouse_host = "clickhouse.${kubernetes_namespace_v1.kubehero.metadata[0].name}.svc.cluster.local"

  postgres_url   = "postgres://${module.postgres.user}:${random_password.postgres.result}@${module.postgres.private_ip}:5432/${module.postgres.database}?sslmode=require"
  clickhouse_url = "clickhouse://kubehero:${random_password.clickhouse.result}@${local.clickhouse_host}:9000/kubehero"
}

# ─── Secret Manager (operator-facing source of truth) ───────────────────────

resource "google_secret_manager_secret" "db_urls" {
  for_each = toset(["postgres", "clickhouse"])

  project   = var.project_id
  secret_id = "${var.cluster_name}-${each.value}-url"

  replication {
    auto {}
  }

  depends_on = [google_project_service.required]
}

resource "google_secret_manager_secret_version" "postgres" {
  secret      = google_secret_manager_secret.db_urls["postgres"].id
  secret_data = local.postgres_url
}

resource "google_secret_manager_secret_version" "clickhouse" {
  secret      = google_secret_manager_secret.db_urls["clickhouse"].id
  secret_data = local.clickhouse_url
}

# ─── Kubernetes secrets (chart-facing) ──────────────────────────────────────

resource "kubernetes_secret_v1" "postgres" {
  metadata {
    name      = "kubehero-postgres-credentials"
    namespace = kubernetes_namespace_v1.kubehero.metadata[0].name
  }

  type = "Opaque"

  data = {
    url      = local.postgres_url
    host     = module.postgres.private_ip
    port     = "5432"
    database = module.postgres.database
    username = module.postgres.user
    password = random_password.postgres.result
  }
}

resource "kubernetes_secret_v1" "clickhouse" {
  metadata {
    name      = "kubehero-clickhouse-credentials"
    namespace = kubernetes_namespace_v1.kubehero.metadata[0].name
  }

  type = "Opaque"

  data = {
    url      = local.clickhouse_url
    host     = local.clickhouse_host
    port     = "9000"
    database = "kubehero"
    username = "kubehero"
    password = random_password.clickhouse.result
  }
}
