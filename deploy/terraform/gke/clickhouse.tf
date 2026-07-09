# ─── ClickHouse: standalone helm_release on a dedicated, tainted pool ───────
#
# Why not the kubehero chart's embedded ClickHouse subchart? It IS the
# simpler wiring (one release, zero extra values), but the chart's own
# docs treat embedded storage as a dev-mode convenience: it mints
# per-install dev credentials, and values.production.yaml explicitly
# ships `clickhouse.embedded: false`. Coupling the datastore's lifecycle
# to app-chart upgrades is also how you turn a routine `helm upgrade`
# into a data migration. So the reference setup runs the same Bitnami
# chart (same version the kubehero chart vendors as its subchart), as
# its own release, pinned to a dedicated node pool — and points KubeHero
# at it through the standard `clickhouse.external.*` values.
#
# Sized as single shard / single replica: KubeHero's ingest volume for a
# handful of clusters fits comfortably. Scale up by growing the node
# machine type first; move to sharding + ClickHouse Keeper (or the
# Altinity operator) only when a single node genuinely saturates.

resource "helm_release" "clickhouse" {
  name       = "clickhouse"
  namespace  = kubernetes_namespace_v1.kubehero.metadata[0].name
  repository = "https://charts.bitnami.com/bitnami"
  chart      = "clickhouse"
  version    = var.clickhouse_chart_version

  wait    = true
  timeout = 600

  values = [
    yamlencode({
      fullnameOverride = "clickhouse" # stable svc name: clickhouse.<ns>.svc

      shards       = 1
      replicaCount = 1

      zookeeper = {
        enabled = false # only needed for replicated tables
      }

      auth = {
        username = "kubehero"
        # password is injected via set_sensitive below
      }

      initdbScripts = {
        "00-create-db.sql" = "CREATE DATABASE IF NOT EXISTS kubehero;"
      }

      persistence = {
        size = var.clickhouse_disk_size
      }

      resources = {
        requests = { cpu = "1", memory = "4Gi" }
        limits   = { cpu = "3", memory = "12Gi" }
      }

      # Pin to the dedicated pool created in main.tf.
      nodeSelector = {
        "kubehero.io/pool" = "clickhouse"
      }
      tolerations = [{
        key      = "dedicated"
        operator = "Equal"
        value    = "clickhouse"
        effect   = "NoSchedule"
      }]
    })
  ]

  set_sensitive = [
    {
      name  = "auth.password"
      value = random_password.clickhouse.result
    }
  ]

  depends_on = [module.gke]
}
