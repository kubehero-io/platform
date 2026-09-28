# KubeHero development inner loop.
# Requires: Tilt, a local K8s (kind / minikube / k3d / Docker Desktop), kubectl.
#
#   tilt up    # watches sources, hot-rebuilds on change
#   tilt down
#
# Flow:
#   1. docker_build — multi-stage Dockerfiles from services/*/Dockerfile,
#      triggered on source change
#   2. helm_resource — installs the chart into the current kube-context,
#      namespace kubehero-system
#   3. port_forwards — everything you need mapped to localhost

load('ext://helm_resource', 'helm_resource', 'helm_repo')

allow_k8s_contexts([
    'kind-kind',
    'kind-kubehero-demo',
    'kind-kubehero-e2e',
    'docker-desktop',
    'minikube',
    'k3d-kubehero',
])

# ─── builds ────────────────────────────────────────────────────────────────
# Go images build from the repo root with GOWORK=off: each needs only
# packages/ plus its own module.
GO_SERVICES = {
    'collector':      'services/collector',
    'control-plane':  'services/control-plane',
    'pricing-engine': 'services/pricing-engine',
    'operator':       'services/operator',
    'advisor':        'services/advisor',
}
for name, path in GO_SERVICES.items():
    docker_build(
        'kubehero/%s:dev' % name,
        '.', dockerfile='%s/Dockerfile' % path,
        only=[path + '/', 'packages/'],
    )
docker_build(
    'kubehero/dashboard:dev',
    '.', dockerfile='apps/dashboard/Dockerfile',
    only=['apps/dashboard/', 'packages/', 'package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml'],
)

# ─── helm install ──────────────────────────────────────────────────────────
helm_resource(
    name='kubehero',
    chart='./deploy/helm/kubehero',
    namespace='kubehero-system',
    flags=[
        '--create-namespace',
        '--set', 'image.registry=',
        '--set', 'image.repository=kubehero',
        '--set', 'image.tag=dev',
        '--set', 'image.pullPolicy=IfNotPresent',
        '--set', 'controlPlane.replicas=1',
        '--set', 'clickhouse.storage.size=4Gi',
        '--set', 'postgresql.storage.size=1Gi',
    ],
    deps=[
        'deploy/helm/kubehero/values.yaml',
        'deploy/helm/kubehero/templates/',
        'deploy/helm/kubehero/crds/',
    ],
    image_deps=['kubehero/%s:dev' % n for n in list(GO_SERVICES.keys()) + ['dashboard']],
)

# ─── port forwards ─────────────────────────────────────────────────────────
k8s_resource('kubehero', port_forwards=[
    port_forward(3001, 3001, name='dashboard'),
    port_forward(8080, 8080, name='control-plane rpc'),
    port_forward(8083, 8083, name='advisor'),
])
