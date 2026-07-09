variable "project_id" {
  description = "GCP project the cluster lives in."
  type        = string
}

variable "name" {
  description = "Cluster name."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,38}[a-z0-9]$", var.name))
    error_message = "Cluster name must be 2-40 chars, lowercase alphanumeric or '-', starting with a letter."
  }
}

variable "region" {
  description = "Region for the regional cluster (control plane replicated across 3 zones)."
  type        = string
}

variable "network" {
  description = "Self-link or name of the VPC network."
  type        = string
}

variable "subnetwork" {
  description = "Self-link or name of the subnetwork (must carry the secondary ranges below)."
  type        = string
}

variable "pods_range_name" {
  description = "Name of the subnetwork secondary range used for pod IPs."
  type        = string
}

variable "services_range_name" {
  description = "Name of the subnetwork secondary range used for service IPs."
  type        = string
}

variable "release_channel" {
  description = "GKE release channel. REGULAR balances freshness and stability."
  type        = string
  default     = "REGULAR"

  validation {
    condition     = contains(["RAPID", "REGULAR", "STABLE"], var.release_channel)
    error_message = "release_channel must be one of RAPID, REGULAR, STABLE."
  }
}

variable "master_ipv4_cidr" {
  description = "RFC1918 /28 for the private control-plane peering range."
  type        = string
  default     = "172.16.0.16/28"

  validation {
    condition     = can(cidrhost(var.master_ipv4_cidr, 0)) && endswith(var.master_ipv4_cidr, "/28")
    error_message = "master_ipv4_cidr must be a valid /28 CIDR block."
  }
}

variable "master_authorized_cidrs" {
  description = <<-EOT
    CIDRs allowed to reach the public control-plane endpoint. Empty list
    means no restriction (any public IP, still authenticated). Lock this
    down to your office/VPN ranges in real deployments.
  EOT
  type = list(object({
    cidr_block   = string
    display_name = string
  }))
  default = []
}

variable "deletion_protection" {
  description = "Refuse `terraform destroy` of the cluster until flipped off."
  type        = bool
  default     = true
}

variable "node_pools" {
  description = <<-EOT
    Node pools keyed by pool name. min/max node counts are TOTAL across
    the region (location_policy handles zone spreading).
  EOT
  type = map(object({
    machine_type = string
    min_nodes    = number
    max_nodes    = number
    spot         = optional(bool, false)
    disk_size_gb = optional(number, 100)
    disk_type    = optional(string, "pd-balanced")
    labels       = optional(map(string), {})
    taints = optional(list(object({
      key    = string
      value  = string
      effect = string # NO_SCHEDULE | PREFER_NO_SCHEDULE | NO_EXECUTE
    })), [])
  }))

  validation {
    condition     = alltrue([for p in var.node_pools : p.min_nodes >= 0 && p.max_nodes >= p.min_nodes])
    error_message = "Each node pool needs 0 <= min_nodes <= max_nodes."
  }
}
