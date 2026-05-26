#!/usr/bin/env bash
# Install the full KubeHero stack on an existing cluster.
# Each step is idempotent; skip whichever your cluster already has.
#
#   ./infra/demo/stack-install.sh                  # interactive, asks which deps
#   ./infra/demo/stack-install.sh --all            # install everything
#   ./infra/demo/stack-install.sh --core-only      # just kubehero + prometheus

set -euo pipefail

MODE="${1:-}"
INTERACTIVE=1
[ "$MODE" = "--all" ] && INTERACTIVE=0 && ALL=1
[ "$MODE" = "--core-only" ] && INTERACTIVE=0 && ALL=0

c_reset="\033[0m"; c_g="\033[32m"; c_b="\033[34m"; c_y="\033[33m"; c_dim="\033[2m"
step() { printf "${c_b}▸${c_reset} %s\n" "$*"; }
ok()   { printf "${c_g}✓${c_reset} %s\n" "$*"; }
skip() { printf "${c_dim}·${c_reset} ${c_dim}%s${c_reset}\n" "$*"; }

ask() {
  local prompt="$1"
  [ $INTERACTIVE -eq 0 ] && { [ $ALL -eq 1 ]; return; }
  read -r -p "$prompt [y/N] " a
  [[ "$a" =~ ^[Yy]$ ]]
}

repo_add() {
  local name="$1" url="$2"
  if helm repo list 2>/dev/null | awk '{print $1}' | grep -qx "$name"; then
    skip "helm repo $name already added"
  else
    helm repo add "$name" "$url" >/dev/null
    ok "added helm repo $name"
  fi
}

# ─── 0. prereqs ──────────────────────────────────────────────────────────
step "checking prerequisites"
for bin in kubectl helm; do
  command -v "$bin" >/dev/null || { echo "missing: $bin"; exit 1; }
done
ok "kubectl + helm present"

# ─── 1. cert-manager ─────────────────────────────────────────────────────
if ask "install cert-manager (mTLS cert rotation)?"; then
  step "cert-manager"
  kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
  kubectl -n cert-manager wait --for=condition=Available deploy --all --timeout=3m
  ok "cert-manager up"
fi

# ─── 2. external-secrets-operator ────────────────────────────────────────
if ask "install external-secrets-operator (AWS SM / Azure KV / GCP SM bridge)?"; then
  step "external-secrets"
  repo_add external-secrets https://charts.external-secrets.io
  helm upgrade --install external-secrets external-secrets/external-secrets \
    --namespace external-secrets --create-namespace --wait
  ok "external-secrets up"
fi

# ─── 3. kube-prometheus-stack ────────────────────────────────────────────
if ask "install kube-prometheus-stack (Prometheus + Grafana + Alertmanager)?"; then
  step "kube-prometheus-stack"
  repo_add prometheus-community https://prometheus-community.github.io/helm-charts
  helm upgrade --install kps prometheus-community/kube-prometheus-stack \
    --namespace monitoring --create-namespace \
    --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
    --set prometheus.prometheusSpec.ruleSelectorNilUsesHelmValues=false \
    --wait --timeout 10m
  ok "kube-prometheus-stack up"
fi

# ─── 4. CloudNativePG operator ───────────────────────────────────────────
if ask "install CloudNativePG (Postgres operator for metadata store)?"; then
  step "cloudnative-pg"
  repo_add cnpg https://cloudnative-pg.github.io/charts
  helm upgrade --install cnpg cnpg/cloudnative-pg \
    --namespace cnpg-system --create-namespace --wait
  ok "cnpg up"
fi

# ─── 5. ClickHouse operator (altinity) ───────────────────────────────────
if ask "install ClickHouse operator (time-series cost store)?"; then
  step "clickhouse-operator"
  kubectl apply -f https://raw.githubusercontent.com/Altinity/clickhouse-operator/master/deploy/operator/clickhouse-operator-install-bundle.yaml
  ok "clickhouse-operator up"
fi

# ─── 6. Valkey (BSD-fork Redis) ──────────────────────────────────────────
if ask "install Valkey (cache + rate-limit)?"; then
  step "valkey"
  repo_add bitnami https://charts.bitnami.com/bitnami
  helm upgrade --install valkey bitnami/valkey \
    --namespace kubehero-system --create-namespace --wait
  ok "valkey up"
fi

# ─── 7. Dex (auth) ───────────────────────────────────────────────────────
if ask "install Dex (OIDC proxy to your IdP)?"; then
  step "dex"
  repo_add dex https://charts.dexidp.io
  helm upgrade --install dex dex/dex \
    --namespace dex --create-namespace --wait \
    --values <(cat <<'YAML'
config:
  issuer: http://dex.dex.svc:5556
  storage: { type: kubernetes, config: { inCluster: true } }
  staticPasswords:
    - email: "admin@kubehero.local"
      hash: "$2a$10$2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYiFPm1leZck7Mc8T4W"  # "admin"
      username: admin
YAML
)
  ok "dex up (dev-password: admin / admin — swap for real IdP connectors in prod)"
fi

# ─── 8. Trivy Operator ───────────────────────────────────────────────────
if ask "install Trivy Operator (CVE + misconfig scans)?"; then
  step "trivy-operator"
  repo_add aqua https://aquasecurity.github.io/helm-charts/
  helm upgrade --install trivy-operator aqua/trivy-operator \
    --namespace trivy-system --create-namespace --wait
  ok "trivy-operator up"
fi

# ─── 9. Tetragon (eBPF runtime security) ─────────────────────────────────
if ask "install Tetragon (optional eBPF runtime security)?"; then
  step "tetragon"
  repo_add cilium https://helm.cilium.io
  helm upgrade --install tetragon cilium/tetragon \
    --namespace kube-system --wait
  ok "tetragon up"
fi

# ─── 10. KubeHero ────────────────────────────────────────────────────────
step "kubehero (this chart)"
helm upgrade --install kubehero deploy/helm/kubehero \
  --namespace kubehero-system --create-namespace \
  --set prometheus.release=kps \
  --wait --timeout 5m
ok "kubehero up"

printf "\n${c_g}STACK READY${c_reset}\n"
printf "  Grafana    kubectl -n monitoring port-forward svc/kps-grafana 3000:80\n"
printf "  Dashboard  kubectl -n kubehero-system port-forward svc/kubehero-dashboard 3001:3001\n"
