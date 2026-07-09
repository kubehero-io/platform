output "instance_name" {
  description = "Cloud SQL instance name."
  value       = google_sql_database_instance.this.name
}

output "connection_name" {
  description = "Connection name for the Cloud SQL Auth Proxy (project:region:instance)."
  value       = google_sql_database_instance.this.connection_name
}

output "private_ip" {
  description = "Private IP address of the instance."
  value       = google_sql_database_instance.this.private_ip_address
}

output "database" {
  description = "Application database name."
  value       = google_sql_database.app.name
}

output "user" {
  description = "Application database user."
  value       = google_sql_user.app.name
}
