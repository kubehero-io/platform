# ─── Project / location ─────────────────────────────────────────────────────

variable "project_id" {
  description = "GCP project to deploy into."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a valid GCP project id (6-30 chars, lowercase letters, digits, hyphens)."
  }
}

variable "region" {
  description = "Region for the cluster, Cloud SQL and networking."
  type        = string
  default     = "us-central1"

  validation {
    condition     = can(regex("^[a-z]+-[a-z]+[0-9]$", var.region))
    error_message = "region must be a GCP region like us-central1 or europe-west4 (not a zone)."
  }
}

variable "cluster_name" {
  description = "Name of the GKE cluster; also prefixes derived resources (SA ids, secrets, SQL instance)."
  type        = string
  default     = "kubehero"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,18}[a-z0-9]$", var.cluster_name))
    error_message = "cluster_name must be 3-20 chars, lowercase alphanumeric or '-', starting with a letter (kept short so derived names stay within GCP limits)."
  }
}

variable "release_channel" {
  description = "GKE release channel."
  type        = string
  default     = "REGULAR"

  validation {
    condition     = contains(["RAPID", "REGULAR", "STABLE"], var.release_channel)
    error_message = "release_channel must be one of RAPID, REGULAR, STABLE."
  }
}

variable "deletion_protection" {
  description = "Protect the cluster and Cloud SQL instance from `terraform destroy`. Flip to false (and apply) before tearing the stack down."
  type        = bool
  default     = true
}

# ─── Networking ─────────────────────────────────────────────────────────────

variable "subnet_cidr" {
  description = "Primary CIDR for the GKE subnetwork (node IPs)."
  type        = string
  default     = "10.10.0.0/20"
}

variable "pods_cidr" {
  description = "Secondary CIDR for pod IPs."
  type        = string
  default     = "10.20.0.0/16"
}

variable "services_cidr" {
  description = "Secondary CIDR for service IPs."
  type        = string
  default     = "10.30.0.0/20"
}

variable "master_ipv4_cidr" {
  description = "/28 for the private GKE control-plane peering."
  type        = string
  default     = "172.16.0.16/28"
}

variable "master_authorized_cidrs" {
  description = "CIDRs allowed to reach the cluster's public API endpoint. Empty = unrestricted (authenticated only). Restrict in production."
  type = list(object({
    cidr_block   = string
    display_name = string
  }))
  default = []
}

# ─── Node pools ─────────────────────────────────────────────────────────────

variable "general_machine_type" {
  description = "Machine type for the on-demand `general` pool (runs KubeHero + system workloads)."
  type        = string
  default     = "e2-standard-4"
}

variable "general_min_nodes" {
  description = "Minimum TOTAL nodes in the general pool (across the region)."
  type        = number
  default     = 3
}

variable "general_max_nodes" {
  description = "Maximum TOTAL nodes in the general pool."
  type        = number
  default     = 6
}

variable "spot_machine_type" {
  description = "Machine type for the `spot` pool (interruptible capacity for batch / stateless work)."
  type        = string
  default     = "e2-standard-4"
}

variable "spot_min_nodes" {
  description = "Minimum TOTAL nodes in the spot pool. 0 lets it scale to nothing."
  type        = number
  default     = 0
}

variable "spot_max_nodes" {
  description = "Maximum TOTAL nodes in the spot pool."
  type        = number
  default     = 10
}

variable "clickhouse_machine_type" {
  description = "Machine type for the dedicated ClickHouse pool. ClickHouse is memory- and IO-hungry; grow this before growing replicas."
  type        = string
  default     = "e2-standard-4"
}

# ─── Cloud SQL (PostgreSQL) ─────────────────────────────────────────────────

variable "postgres_tier" {
  description = "Cloud SQL tier for the metadata database."
  type        = string
  default     = "db-custom-2-8192"
}

variable "postgres_availability_type" {
  description = "ZONAL (default, cheaper) or REGIONAL (HA standby, ~2x cost)."
  type        = string
  default     = "ZONAL"

  validation {
    condition     = contains(["ZONAL", "REGIONAL"], var.postgres_availability_type)
    error_message = "postgres_availability_type must be ZONAL or REGIONAL."
  }
}

variable "postgres_database" {
  description = "Application database name."
  type        = string
  default     = "kubehero"
}

# ─── Helm releases ──────────────────────────────────────────────────────────

variable "kubehero_chart_version" {
  description = "Version of the kubehero chart pulled from oci://ghcr.io/kubehero-io/charts."
  type        = string
  default     = "0.1.0"
}

variable "clickhouse_chart_version" {
  description = "Bitnami ClickHouse chart version. Kept in lock-step with the version vendored as the kubehero chart's embedded subchart (deploy/helm/kubehero/Chart.lock)."
  type        = string
  default     = "7.2.0"
}

variable "clickhouse_disk_size" {
  description = "PVC size for ClickHouse data."
  type        = string
  default     = "100Gi"

  validation {
    condition     = can(regex("^[0-9]+(Gi|Ti)$", var.clickhouse_disk_size))
    error_message = "clickhouse_disk_size must look like 100Gi or 1Ti."
  }
}

variable "enable_gcp_cost_adapter" {
  description = "Create a Workload Identity-bound GSA and enable the chart's GCP cloud adapter so KubeHero can read pricing/monitoring data for this project."
  type        = bool
  default     = true
}
