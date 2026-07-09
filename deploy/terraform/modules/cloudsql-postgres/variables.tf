variable "project_id" {
  description = "GCP project for the instance."
  type        = string
}

variable "name" {
  description = "Cloud SQL instance name. NOTE: instance names cannot be reused for ~7 days after deletion."
  type        = string
}

variable "region" {
  description = "Region for the instance (co-locate with the GKE cluster)."
  type        = string
}

variable "database_version" {
  description = "PostgreSQL major version."
  type        = string
  default     = "POSTGRES_16"

  validation {
    condition     = can(regex("^POSTGRES_1[4-9]$", var.database_version))
    error_message = "database_version must be POSTGRES_14 .. POSTGRES_19."
  }
}

variable "tier" {
  description = "Machine tier, e.g. db-custom-2-8192 (2 vCPU / 8 GiB)."
  type        = string
  default     = "db-custom-2-8192"

  validation {
    condition     = startswith(var.tier, "db-")
    error_message = "tier must be a Cloud SQL tier (db-custom-*, db-perf-optimized-*, ...)."
  }
}

variable "availability_type" {
  description = "ZONAL (single zone, cheaper) or REGIONAL (HA standby, ~2x cost)."
  type        = string
  default     = "ZONAL"

  validation {
    condition     = contains(["ZONAL", "REGIONAL"], var.availability_type)
    error_message = "availability_type must be ZONAL or REGIONAL."
  }
}

variable "disk_size_gb" {
  description = "Initial SSD size in GiB (autoresize is on, so this is a floor)."
  type        = number
  default     = 20

  validation {
    condition     = var.disk_size_gb >= 10
    error_message = "disk_size_gb must be at least 10."
  }
}

variable "network_id" {
  description = "VPC network id for the private IP (requires an established Private Service Access connection — pass it via depends_on / psa_dependency)."
  type        = string
}

variable "database" {
  description = "Application database to create."
  type        = string
  default     = "kubehero"
}

variable "user" {
  description = "Application database user to create."
  type        = string
  default     = "kubehero"
}

variable "password" {
  description = "Password for the application user (generate with random_password; never commit)."
  type        = string
  sensitive   = true
}

variable "deletion_protection" {
  description = "Refuse `terraform destroy` of the instance until flipped off."
  type        = bool
  default     = true
}
