# ─── Least-privilege node service account ──────────────────────────────────
# Nodes never run as the Compute Engine default SA. The pool SA gets only
# what kubelet + logging/monitoring agents need, plus registry pull.

resource "google_service_account" "nodes" {
  project      = var.project_id
  account_id   = "${var.name}-nodes"
  display_name = "GKE node SA for ${var.name}"
}

resource "google_project_iam_member" "nodes" {
  for_each = toset([
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/monitoring.viewer",
    "roles/stackdriver.resourceMetadata.writer",
    "roles/artifactregistry.reader",
  ])

  project = var.project_id
  role    = each.value
  member  = google_service_account.nodes.member
}

# ─── Regional cluster ───────────────────────────────────────────────────────

resource "google_container_cluster" "this" {
  project  = var.project_id
  name     = var.name
  location = var.region # region (not zone) → HA control plane across 3 zones

  network    = var.network
  subnetwork = var.subnetwork

  # VPC-native with Dataplane V2 (eBPF) — required for the chart's
  # `networkPolicies.enabled=true` to actually enforce anything.
  networking_mode   = "VPC_NATIVE"
  datapath_provider = "ADVANCED_DATAPATH"

  ip_allocation_policy {
    cluster_secondary_range_name  = var.pods_range_name
    services_secondary_range_name = var.services_range_name
  }

  release_channel {
    channel = var.release_channel
  }

  # Workload Identity: pods exchange KSA tokens for GSA credentials —
  # no exported service-account keys anywhere in this stack.
  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  # Private nodes (egress via Cloud NAT, managed in the root config);
  # public control-plane endpoint so `terraform apply` and CI can reach
  # the API without a bastion.
  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = false
    master_ipv4_cidr_block  = var.master_ipv4_cidr
  }

  dynamic "master_authorized_networks_config" {
    for_each = length(var.master_authorized_cidrs) > 0 ? [1] : []
    content {
      dynamic "cidr_blocks" {
        for_each = var.master_authorized_cidrs
        content {
          cidr_block   = cidr_blocks.value.cidr_block
          display_name = cidr_blocks.value.display_name
        }
      }
    }
  }

  # We manage pools explicitly below; drop the throwaway default pool.
  remove_default_node_pool = true
  initial_node_count       = 1

  # KubeHero consumes GKE cost-allocation metering — turn it on.
  cost_management_config {
    enabled = true
  }

  maintenance_policy {
    daily_maintenance_window {
      start_time = "03:00"
    }
  }

  deletion_protection = var.deletion_protection
}

# ─── Node pools ─────────────────────────────────────────────────────────────

resource "google_container_node_pool" "pools" {
  for_each = var.node_pools

  project  = var.project_id
  name     = each.key
  location = var.region
  cluster  = google_container_cluster.this.name

  autoscaling {
    total_min_node_count = each.value.min_nodes
    total_max_node_count = each.value.max_nodes
    # Spot capacity is found wherever it exists; on-demand spreads evenly.
    location_policy = each.value.spot ? "ANY" : "BALANCED"
  }

  node_config {
    machine_type = each.value.machine_type
    spot         = each.value.spot
    disk_size_gb = each.value.disk_size_gb
    disk_type    = each.value.disk_type

    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]

    labels = each.value.labels

    dynamic "taint" {
      for_each = each.value.taints
      content {
        key    = taint.value.key
        value  = taint.value.value
        effect = taint.value.effect
      }
    }

    workload_metadata_config {
      mode = "GKE_METADATA"
    }

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }

    metadata = {
      disable-legacy-endpoints = "true"
    }
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  upgrade_settings {
    max_surge       = 1
    max_unavailable = 0
  }
}
