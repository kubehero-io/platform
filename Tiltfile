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
load('ext://restart_process', 'docker_build_with_restart')

allow_k8s_contexts([
    'kind-kind',
    'kind-kubehero-demo',
    'docker-desktop',
    'minikube',
    'k3d-kubehero',
])

# ─── builds ────────────────────────────────────────────────────────────────
docker_build(
    'kubehero/collector:dev',
    '.', dockerfile='services/collector/Dockerfile',
    only=['services/collector/', 'packages/proto/'],
)
docker_build(
    'kubehero/control-plane:dev',
    '.', dockerfile='services/control-plane/Dockerfile',
    only=['services/control-plane/', 'packages/proto/'],
)
docker_build(
    'kubehero/pricing-engine:dev',
    '.', dockerfile='services/pricing-engine/Dockerfile',
    only=['services/pricing-engine/', 'packages/proto/'],
)
docker_build(
    'kubehero/operator:dev',
    'services/operator', dockerfile='services/operator/Dockerfile',
)
docker_build(
    'kubehero/dashboard:dev',
    '.', dockerfile='apps/dashboard/Dockerfile',
    only=['apps/dashboard/', 'package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml'],
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
        '--set', 'collector.image.tag=dev',
        '--set', 'controlPlane.image.tag=dev',
        '--set', 'pricingEngine.image.tag=dev',
        '--set', 'operator.image.tag=dev',
        '--set', 'dashboard.image.tag=dev',
        '--set', 'image.pullPolicy=IfNotPresent',
    ],
    deps=[
        'deploy/helm/kubehero/values.yaml',
        'deploy/helm/kubehero/templates/',
        'deploy/helm/kubehero/crds/',
    ],
    image_deps=[
        'kubehero/collector:dev',
        'kubehero/control-plane:dev',
        'kubehero/pricing-engine:dev',
        'kubehero/operator:dev',
        'kubehero/dashboard:dev',
    ],
)

# ─── port forwards ─────────────────────────────────────────────────────────
k8s_resource('kubehero', port_forwards=[
    port_forward(3001, 3001, name='dashboard'),
    port_forward(8080, 8080, name='control-plane rpc'),
    port_forward(8081, 8081, name='collector metrics'),
])
