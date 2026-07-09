provider "google" {
  project = var.project_id
  region  = var.region
}

# Kubernetes + Helm authenticate with the caller's ADC token against the
# cluster this same config creates. Works in a single apply because the
# provider config is only resolved once module.gke has produced outputs.
data "google_client_config" "current" {}

provider "kubernetes" {
  host                   = "https://${module.gke.endpoint}"
  token                  = data.google_client_config.current.access_token
  cluster_ca_certificate = base64decode(module.gke.ca_certificate)
}

provider "helm" {
  kubernetes = {
    host                   = "https://${module.gke.endpoint}"
    token                  = data.google_client_config.current.access_token
    cluster_ca_certificate = base64decode(module.gke.ca_certificate)
  }
}
