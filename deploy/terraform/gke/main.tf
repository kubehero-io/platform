# ─── GKE: regional cluster, 3 pools ─────────────────────────────────────────
#
#   general    — on-demand; KubeHero control plane, dashboard, system pods
#   spot       — interruptible; the pool the collector meters savings on.
#                GKE labels these nodes cloud.google.com/gke-spot=true
#                automatically. Intentionally untainted so ordinary
#                stateless workloads can land here; taint it if you want
#                explicit opt-in.
#   clickhouse — tainted, single-purpose pool so the time-series store
#                never competes with (or gets evicted by) app workloads.

module "gke" {
  source = "../modules/gke-cluster"

  project_id = var.project_id
  name       = var.cluster_name
  region     = var.region

  network             = google_compute_network.vpc.id
  subnetwork          = google_compute_subnetwork.gke.id
  pods_range_name     = "pods"
  services_range_name = "services"

  release_channel         = var.release_channel
  master_ipv4_cidr        = var.master_ipv4_cidr
  master_authorized_cidrs = var.master_authorized_cidrs
  deletion_protection     = var.deletion_protection

  node_pools = {
    general = {
      machine_type = var.general_machine_type
      min_nodes    = var.general_min_nodes
      max_nodes    = var.general_max_nodes
      labels       = { "kubehero.io/pool" = "general" }
    }

    spot = {
      machine_type = var.spot_machine_type
      min_nodes    = var.spot_min_nodes
      max_nodes    = var.spot_max_nodes
      spot         = true
      labels       = { "kubehero.io/pool" = "spot" }
    }

    clickhouse = {
      machine_type = var.clickhouse_machine_type
      min_nodes    = 1
      max_nodes    = 3
      disk_type    = "pd-ssd"
      labels       = { "kubehero.io/pool" = "clickhouse" }
      taints = [{
        key    = "dedicated"
        value  = "clickhouse"
        effect = "NO_SCHEDULE"
      }]
    }
  }

  depends_on = [google_project_service.required]
}

# ─── Cloud SQL: private-IP PostgreSQL for control-plane metadata ────────────

module "postgres" {
  source = "../modules/cloudsql-postgres"

  project_id = var.project_id
  name       = "${var.cluster_name}-pg"
  region     = var.region

  tier              = var.postgres_tier
  availability_type = var.postgres_availability_type
  database          = var.postgres_database
  user              = "kubehero"
  password          = random_password.postgres.result

  network_id          = google_compute_network.vpc.id
  deletion_protection = var.deletion_protection

  # The private IP can only be allocated once the PSA peering exists.
  depends_on = [google_service_networking_connection.psa]
}
