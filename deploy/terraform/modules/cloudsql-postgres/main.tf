resource "google_sql_database_instance" "this" {
  project          = var.project_id
  name             = var.name
  region           = var.region
  database_version = var.database_version

  # Instance-level guard against accidental terraform destroy.
  deletion_protection = var.deletion_protection

  settings {
    tier              = var.tier
    edition           = "ENTERPRISE"
    availability_type = var.availability_type

    disk_type       = "PD_SSD"
    disk_size       = var.disk_size_gb
    disk_autoresize = true

    # Private IP only — no public ipv4, TLS enforced on the wire.
    ip_configuration {
      ipv4_enabled    = false
      private_network = var.network_id
      ssl_mode        = "ENCRYPTED_ONLY"
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
      start_time                     = "02:00"
      transaction_log_retention_days = 7

      backup_retention_settings {
        retained_backups = 14
        retention_unit   = "COUNT"
      }
    }

    maintenance_window {
      day          = 7 # Sunday
      hour         = 3
      update_track = "stable"
    }

    insights_config {
      query_insights_enabled  = true
      record_application_tags = false
      record_client_address   = false
    }

    # API-level guard (in addition to the Terraform-level flag above).
    deletion_protection_enabled = var.deletion_protection
  }
}

resource "google_sql_database" "app" {
  project  = var.project_id
  instance = google_sql_database_instance.this.name
  name     = var.database
}

resource "google_sql_user" "app" {
  project  = var.project_id
  instance = google_sql_database_instance.this.name
  name     = var.user
  password = var.password
}
