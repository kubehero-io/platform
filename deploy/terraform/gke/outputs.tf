# ─── Cluster access ─────────────────────────────────────────────────────────

output "cluster_name" {
  description = "GKE cluster name."
  value       = module.gke.name
}

output "region" {
  description = "Deployment region."
  value       = var.region
}

output "kubeconfig_command" {
  description = "Run this to add the cluster to your kubeconfig."
  value       = "gcloud container clusters get-credentials ${module.gke.name} --region ${var.region} --project ${var.project_id}"
}

output "gke_endpoint" {
  description = "Cluster control-plane endpoint."
  value       = module.gke.endpoint
}

# ─── Datastores ─────────────────────────────────────────────────────────────

output "postgres_connection_name" {
  description = "Cloud SQL connection name (for the Auth Proxy)."
  value       = module.postgres.connection_name
}

output "postgres_private_ip" {
  description = "Private IP of the PostgreSQL instance."
  value       = module.postgres.private_ip
}

output "clickhouse_endpoint" {
  description = "In-cluster ClickHouse endpoint (native TCP)."
  value       = "${local.clickhouse_host}:9000"
}

output "secret_manager_secrets" {
  description = "Secret Manager ids holding the DB connection URLs (break-glass access: gcloud secrets versions access latest --secret <id>)."
  value       = { for k, s in google_secret_manager_secret.db_urls : k => s.secret_id }
}

# ─── App access ─────────────────────────────────────────────────────────────

output "dashboard_hint" {
  description = "Reach the KubeHero dashboard without an Ingress."
  value       = "kubectl -n kubehero port-forward svc/kubehero-dashboard 3001:3001  # then open http://localhost:3001"
}

output "api_hint" {
  description = "Reach the control-plane API without an Ingress."
  value       = "kubectl -n kubehero port-forward svc/kubehero-control-plane 8080:8080"
}
