output "name" {
  description = "Cluster name."
  value       = google_container_cluster.this.name
}

output "id" {
  description = "Fully qualified cluster id (projects/.../locations/.../clusters/...)."
  value       = google_container_cluster.this.id
}

output "endpoint" {
  description = "Control-plane endpoint IP (public)."
  value       = google_container_cluster.this.endpoint
}

output "ca_certificate" {
  description = "Base64-encoded cluster CA certificate."
  value       = google_container_cluster.this.master_auth[0].cluster_ca_certificate
  sensitive   = true
}

output "workload_pool" {
  description = "Workload Identity pool (\"<project>.svc.id.goog\")."
  value       = google_container_cluster.this.workload_identity_config[0].workload_pool
}

output "node_service_account_email" {
  description = "Email of the node service account."
  value       = google_service_account.nodes.email
}

output "node_pool_names" {
  description = "Names of the managed node pools."
  value       = [for p in google_container_node_pool.pools : p.name]
}
