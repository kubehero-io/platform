terraform {
  required_version = ">= 1.9.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.39"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.2"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.2"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
  }

  # ─── Remote state ────────────────────────────────────────────────────────
  # Uncomment after creating a versioned GCS bucket (see ../README.md):
  #
  #   gcloud storage buckets create gs://YOUR-TF-STATE-BUCKET \
  #     --location=us --uniform-bucket-level-access
  #   gcloud storage buckets update gs://YOUR-TF-STATE-BUCKET --versioning
  #
  # backend "gcs" {
  #   bucket = "YOUR-TF-STATE-BUCKET"
  #   prefix = "kubehero/gke"
  # }
}
