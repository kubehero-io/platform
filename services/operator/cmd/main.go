package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	kubeherov1 "github.com/kubehero-io/platform/services/operator/api/v1"
	"github.com/kubehero-io/platform/services/operator/internal/controller"
	"github.com/kubehero-io/platform/services/operator/internal/escalator"
	// +kubebuilder:scaffold:imports
)

// escalatorAudit bridges escalator.AuditSink → controller.AuditEmitter so
// the runner can post per-step events without escalator depending on
// controller. We translate the AuditEvent struct between the two
// packages — same fields, same wire shape.
type escalatorAudit struct{ inner controller.AuditEmitter }

func (e escalatorAudit) Emit(ctx context.Context, ev escalator.AuditEvent) error {
	return e.inner.Emit(ctx, controller.AuditEvent{
		Org:            ev.Org,
		ClusterID:      ev.ClusterID,
		ActorSub:       ev.ActorSub,
		ActorEmail:     ev.ActorEmail,
		Action:         ev.Action,
		TargetKind:     ev.TargetKind,
		TargetName:     ev.TargetName,
		Payload:        ev.Payload,
		PayloadJSON:    ev.PayloadJSON,
		Outcome:        ev.Outcome,
		EffectUsdMonth: ev.EffectUsdMonth,
	})
}

// escalatorAdapter exposes escalator.Runner behind controller's
// EscalationRunner interface (decouples the two packages).
type escalatorAdapter struct{ inner *escalator.Runner }

func (a escalatorAdapter) Execute(
	ctx context.Context,
	policy *kubeherov1.CeilingPolicy,
	clusterID, namespaceForActions string,
) []controller.EscalationResult {
	results := a.inner.Execute(ctx, policy, clusterID, namespaceForActions)
	out := make([]controller.EscalationResult, len(results))
	for i, r := range results {
		out[i] = controller.EscalationResult{Action: r.Action, Outcome: r.Outcome, Message: r.Message}
	}
	return out
}

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(kubeherov1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "c4272711.kubehero.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := (&controller.BudgetPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "BudgetPolicy")
		os.Exit(1)
	}
	// Audit emitter: when CONTROL_PLANE_URL is set, the operator posts
	// fired-policy events into the cp's append-only audit log. When unset
	// it falls back to a no-op so the kind demo profile and unit tests
	// don't need a control-plane peer.
	auditEmitter := controller.AuditEmitter(controller.NoopAuditEmitter{})
	if cp := os.Getenv("CONTROL_PLANE_URL"); cp != "" {
		auditEmitter = &controller.HTTPAuditEmitter{
			Endpoint: cp,
			Token:    os.Getenv("CONTROL_PLANE_TOKEN"),
		}
	}

	// Burn-rate provider: when CONTROL_PLANE_URL is set, ask the cp via
	// RPC (which reads ClickHouse). Otherwise stay in stub mode so dev
	// + kind-demo never trip a policy without real data.
	var burnRate controller.BurnRateProvider = controller.StubBurnRate{}
	if cp := os.Getenv("CONTROL_PLANE_URL"); cp != "" {
		k8s := mgr.GetClient()
		burnRate = &controller.RPCBurnRate{
			Endpoint:  cp,
			Token:     os.Getenv("CONTROL_PLANE_TOKEN"),
			ClusterID: os.Getenv("CLUSTER_ID"),
			CeilingResolver: func(ctx context.Context, namespace, budgetRef string) (float64, error) {
				bp := &kubeherov1.BudgetPolicy{}
				if err := k8s.Get(ctx, k8sclient.ObjectKey{Namespace: namespace, Name: budgetRef}, bp); err != nil {
					return 0, err
				}
				return controller.ParseCeilingUSD(bp.Spec.Ceiling), nil
			},
		}
	}

	// Escalation runner — the actuator is always wired (it talks to
	// the local cluster the operator runs in). The adapter layer below
	// translates our local AuditEvent shape to the controller package's
	// emitter so per-step events land in the same audit log as the
	// "ceiling.tripped" event itself.
	escalationRunner := &escalator.Runner{
		Actuator: &escalator.Actuator{Client: mgr.GetClient()},
		Audit:    escalatorAudit{inner: auditEmitter},
	}

	if err := (&controller.CeilingPolicyReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		BurnRate:  burnRate,
		Audit:     auditEmitter,
		Escalator: escalatorAdapter{inner: escalationRunner},
		ClusterID: os.Getenv("CLUSTER_ID"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "CeilingPolicy")
		os.Exit(1)
	}
	if err := (&controller.RightsizingPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "RightsizingPolicy")
		os.Exit(1)
	}

	// Trivy VulnerabilityReport ingest — opt in via TRIVY_INGEST=true so
	// we don't error on clusters without the Trivy CRD installed.
	if os.Getenv("TRIVY_INGEST") == "true" {
		if err := (&controller.VulnerabilityReportReconciler{
			Client:        mgr.GetClient(),
			Audit:         auditEmitter,
			SeverityFloor: getenv("TRIVY_SEVERITY_FLOOR", "high"),
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create controller", "controller", "VulnerabilityReport")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
