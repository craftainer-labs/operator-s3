// Package app builds and runs the operator's manager. cmd/main.go is a
// three-line wrapper around Run, so everything with a branch in it is here
// and under the coverage gate.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (OIDC, exec, ...) so the
	// manager can use any kubeconfig outside a cluster.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/binding"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
	"github.com/craftainer-labs/lib-operator-go/pkg/probe"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/controller"
	"github.com/craftainer-labs/operator-s3/internal/egress"
	webhookv1alpha1 "github.com/craftainer-labs/operator-s3/internal/webhook/v1alpha1"
	// +kubebuilder:scaffold:imports
)

// LeaderElectionID names the Lease the replicas elect a leader through. An
// instance renames it with the operator.
const LeaderElectionID = "operator-s3.craftainer.dev"

// EventRecorderName is the reporting controller of the operator's Events.
const EventRecorderName = "operator-s3"

var scheme = newScheme()

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(s3v1alpha1.AddToScheme(s))
	// +kubebuilder:scaffold:scheme
	return s
}

// settings are the command-line flags.
type settings struct {
	metricsAddr      string
	secureMetrics    bool
	metricsCertPath  string
	metricsCertName  string
	metricsCertKey   string
	probeAddr        string
	probeOnly        bool
	leaderElect      bool
	leaderElectionNS string
	webhookPort      int
	webhookCertPath  string
	webhookCertName  string
	webhookCertKey   string
	enableHTTP2      bool
	egressMode       string
	deletionPolicy   string
	operatorNS       string
	zap              zap.Options
}

func parseFlags(args []string, stderr io.Writer) (*settings, error) {
	s := &settings{}
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&s.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	fs.BoolVar(&s.secureMetrics, "metrics-secure", true,
		"Serve metrics over HTTPS with token authentication and authorization (TokenReview and SubjectAccessReview). "+
			"Use --metrics-secure=false for plain HTTP.")
	fs.StringVar(&s.metricsCertPath, "metrics-cert-path", "", "The directory that contains the metrics server certificate. "+
		"Empty: controller-runtime generates a self-signed one.")
	fs.StringVar(&s.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	fs.StringVar(&s.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	fs.StringVar(&s.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	fs.BoolVar(&s.probeOnly, "probe-only", os.Getenv("MANAGER_PROBE_ONLY") == "true",
		"Serve only /healthz and /readyz and never contact a cluster (the image smoke test). Default: $MANAGER_PROBE_ONLY.")
	fs.BoolVar(&s.leaderElect, "leader-elect", false, "Enable leader election, so only one replica reconciles.")
	fs.StringVar(&s.leaderElectionNS, "leader-election-namespace", "",
		"The namespace of the leader election Lease. Empty: the pod's namespace (needs the service account).")
	fs.IntVar(&s.webhookPort, "webhook-port", 9443, "Port the webhook server listens on. Set -1 to disable the webhooks.")
	fs.StringVar(&s.webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	fs.StringVar(&s.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	fs.StringVar(&s.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	fs.BoolVar(&s.enableHTTP2, "enable-http2", false, "Enable HTTP/2 on the metrics and webhook servers.")
	fs.StringVar(&s.egressMode, "egress-mode", os.Getenv(egress.EnvMode),
		"Which destinations the object store connections may not reach: strict (loopback, link-local, cloud metadata and private ranges) "+
			"or allow-private (all but the private ranges). Empty means strict. Default: $"+egress.EnvMode+".")
	fs.StringVar(&s.deletionPolicy, "default-data-deletion-policy", os.Getenv(lifecycle.EnvDefaultDataDeletionPolicy),
		"The deletionPolicy a new Bucket gets pinned to when its spec leaves it unset: Retain (the default) or Delete. Default: $"+
			lifecycle.EnvDefaultDataDeletionPolicy+".")
	fs.StringVar(&s.operatorNS, "operator-namespace", os.Getenv(server.OperatorNamespaceEnv),
		"The namespace cluster stores' admin Secrets and CA bundles are read from. Empty: the pod's namespace (needs the service account). Default: $"+
			server.OperatorNamespaceEnv+".")
	s.zap.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return s, nil
}

// tlsOptions are applied to the metrics and webhook servers: TLS 1.2 or
// newer, and no HTTP/2 unless asked for (the Rapid Reset CVEs,
// GHSA-qppj-fm5r-hxr3 and GHSA-4374-p667-p6c8).
func (s *settings) tlsOptions() []func(*tls.Config) {
	opts := []func(*tls.Config){func(c *tls.Config) { c.MinVersion = tls.VersionTLS12 }}
	if !s.enableHTTP2 {
		opts = append(opts, func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} })
	}
	return opts
}

func (s *settings) metricsOptions() metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   s.metricsAddr,
		SecureServing: s.secureMetrics,
		TLSOpts:       s.tlsOptions(),
	}
	if s.secureMetrics {
		// Only callers RBAC lets `get` the /metrics non-resource URL are served.
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if s.metricsCertPath != "" {
		opts.CertDir = s.metricsCertPath
		opts.CertName = s.metricsCertName
		opts.KeyName = s.metricsCertKey
	}
	return opts
}

func (s *settings) webhookServer() webhook.Server {
	opts := webhook.Options{Port: s.webhookPort, TLSOpts: s.tlsOptions()}
	if s.webhookCertPath != "" {
		opts.CertDir = s.webhookCertPath
		opts.CertName = s.webhookCertName
		opts.KeyName = s.webhookCertKey
	}
	return webhook.NewServer(opts)
}

// serviceAccountNamespaceFile holds the pod's namespace; a variable so a test
// can point it elsewhere.
var serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// ErrNoOperatorNamespace means the namespace of the operator could not be
// determined: neither --operator-namespace (or $POD_NAMESPACE), nor the service account of the pod
// names it.
var ErrNoOperatorNamespace = errors.New("cannot determine the operator namespace: set --operator-namespace or POD_NAMESPACE, or run in a pod with a service account")

// controllerSettings are the flags the controllers need, validated.
type controllerSettings struct {
	egress    egress.Policy
	deletion  common.DeletionPolicy
	namespace string
}

// controllerSettings validates the egress mode and the default deletion policy
// (an invalid value fails startup, so the operator never runs with a setting
// nobody chose) and finds the operator namespace.
func (s *settings) controllerSettings() (controllerSettings, error) {
	mode, err := egress.ParseMode(s.egressMode)
	if err != nil {
		return controllerSettings{}, fmt.Errorf("%s: %w", egress.EnvMode, err)
	}
	policy, err := lifecycle.ParseDefaultDeletionPolicy(s.deletionPolicy)
	if err != nil {
		return controllerSettings{}, err
	}
	ns := s.operatorNS
	if ns == "" {
		if b, err := os.ReadFile(serviceAccountNamespaceFile); err == nil {
			ns = strings.TrimSpace(string(b))
		}
	}
	if ns == "" {
		// Every ClusterObjectStore is resolved in this namespace, so an empty
		// one would fail each reconcile with a confusing error instead.
		return controllerSettings{}, ErrNoOperatorNamespace
	}
	ctrl.Log.WithName("setup").Info("controller settings", "egressMode", string(mode),
		"defaultDataDeletionPolicy", string(policy), "operatorNamespace", ns)
	return controllerSettings{egress: egress.NewPolicy(mode), deletion: policy, namespace: ns}, nil
}

// Option adjusts the manager's options; tests use it, the binary passes none.
type Option func(*ctrl.Options)

func (s *settings) managerOptions(opts []Option) ctrl.Options {
	o := ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 s.metricsOptions(),
		HealthProbeBindAddress:  s.probeAddr,
		LeaderElection:          s.leaderElect,
		LeaderElectionID:        LeaderElectionID,
		LeaderElectionNamespace: s.leaderElectionNS,
		// The process exits as soon as the manager stops, so giving up the
		// Lease on cancel is safe and makes a rollout faster.
		LeaderElectionReleaseOnCancel: true,
		// The Secret cache holds only the binding Secrets the operator owns
		// (EB-2), and the ConfigMap cache (the probes' watches) only the
		// ConfigMaps with the same label, so the operator never lists or
		// watches the cluster's other ones; the admin Secrets and CA bundles
		// of stores are read live.
		Cache: cache.Options{ByObject: cacheByObject()},
		Client: client.Options{Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Secret{}, &corev1.ConfigMap{}},
		}},
	}
	if s.webhookPort >= 0 {
		o.WebhookServer = s.webhookServer()
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// checker is the part of ctrl.Manager that registers probes.
type checker interface {
	AddHealthzCheck(name string, check healthz.Checker) error
	AddReadyzCheck(name string, check healthz.Checker) error
}

func addChecks(mgr checker) error {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add ready check: %w", err)
	}
	return nil
}

// Run starts the operator and blocks until ctx is cancelled or the manager
// fails. getConfig is only called when a cluster is needed.
func Run(ctx context.Context, args []string, getConfig func() (*rest.Config, error), stderr io.Writer, opts ...Option) error {
	s, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	// Structured JSON logs by default (EB-7); --zap-devel switches to console.
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&s.zap), zap.WriteTo(stderr)))

	if s.probeOnly {
		return serveProbes(ctx, s.probeAddr)
	}

	cs, err := s.controllerSettings()
	if err != nil {
		return err
	}
	cfg, err := getConfig()
	if err != nil {
		return fmt.Errorf("load the Kubernetes client configuration: %w", err)
	}
	mgr, err := ctrl.NewManager(cfg, s.managerOptions(opts))
	if err != nil {
		return fmt.Errorf("create the manager: %w", err)
	}
	if err := setupManager(mgr, s.webhookPort >= 0, cs); err != nil {
		return err
	}
	ctrl.Log.WithName("setup").Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run the manager: %w", err)
	}
	return nil
}

type setupStep struct {
	name string
	fn   func(ctrl.Manager) error
}

// The manager's access to the group's kinds, and to what the controllers touch
// outside it. Namespaces: allowedNamespaces selectors (server.Resolve).
// Secrets: the stores' admin credentials and CA bundles are read (an
// ObjectStore's live in its own namespace, so this is a ClusterRole rule, not
// only a Role in the operator namespace) and the binding Secrets are written;
// the manager's cache of Secrets is restricted to its managed-by label
// (binding.CacheByObject), and reads of unlabelled Secrets go to the API
// server. The store probes (probe.Reconciler) also watch Secrets and
// ConfigMaps, so those need list and watch too (the ConfigMap cache is
// restricted to the managed-by label like the Secret cache). ConfigMaps: CA
// bundles only. Events: the Event recorder.
//
// +kubebuilder:rbac:groups=s3.craftainer.dev,resources=clusterobjectstores;objectstores,verbs=get;list;watch
// +kubebuilder:rbac:groups=s3.craftainer.dev,resources=buckets;bucketaccesses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=s3.craftainer.dev,resources=clusterobjectstores/status;objectstores/status;buckets/status;bucketaccesses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=s3.craftainer.dev,resources=buckets/finalizers;bucketaccesses/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core;events.k8s.io,resources=events,verbs=create;patch

// storeCacheIdle is how long a store's client stays cached without use.
const storeCacheIdle = 10 * time.Minute

// setupManager is a variable so a test can make the set-up fail: no step can
// fail on a healthy manager.
var setupManager = setup

// setup registers the probes, the webhooks when they are enabled, and the
// controllers.
func setup(mgr ctrl.Manager, webhooks bool, cs controllerSettings) error {
	steps := []setupStep{
		{"probes", func(mgr ctrl.Manager) error { return addChecks(mgr) }},
	}
	if webhooks {
		steps = append(steps,
			setupStep{"cluster object store webhook", webhookv1alpha1.SetupClusterObjectStoreWebhookWithManager},
			setupStep{"object store webhook", webhookv1alpha1.SetupObjectStoreWebhookWithManager},
			setupStep{"bucket webhook", webhookv1alpha1.SetupBucketWebhookWithManager},
			setupStep{"bucket access webhook", webhookv1alpha1.SetupBucketAccessWebhookWithManager},
		)
	}
	cfg := controller.Config{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Recorder: mgr.GetEventRecorder(EventRecorderName),
		ManagedBy: EventRecorderName, OperatorNamespace: cs.namespace, DefaultDeletionPolicy: cs.deletion,
		Egress: cs.egress, NewStore: controller.NewClientStore, Cache: controller.NewStoreCache(storeCacheIdle),
	}
	steps = append(steps,
		setupStep{"Bucket controller", (&controller.BucketReconciler{Config: cfg}).SetupWithManager},
		setupStep{"BucketAccess controller", (&controller.AccessReconciler{Config: cfg}).SetupWithManager},
		setupStep{"ClusterObjectStore probe", storeProbe(mgr, cfg, cs.namespace,
			func() probe.Server { return &s3v1alpha1.ClusterObjectStore{} })},
		setupStep{"ObjectStore probe", storeProbe(mgr, cfg, cs.namespace,
			func() probe.Server { return &s3v1alpha1.ObjectStore{} })})
	// +kubebuilder:scaffold:builder
	return runSteps(mgr, steps)
}

// cacheByObject restricts the Secret and the ConfigMap cache to the objects
// that carry the operator's managed-by label.
func cacheByObject() map[client.Object]cache.ByObject {
	byObject := binding.CacheByObject(EventRecorderName)
	byObject[&corev1.ConfigMap{}] = cache.ByObject{
		Label: labels.SelectorFromSet(labels.Set{binding.LabelManagedBy: EventRecorderName}),
	}
	return byObject
}

// storeProbe returns the setup of the probe.Reconciler of one store kind,
// which writes the kind's status (serverVersion, lastProbeTime, Ready and
// ServerReachable) from a RustFS admin probe. Its Client reads Secrets and
// ConfigMaps live (managerOptions), and its watches on them go through the
// manager's caches, which hold only the managed-by labelled ones.
func storeProbe(mgr ctrl.Manager, cfg controller.Config, operatorNS string, kind func() probe.Server) func(ctrl.Manager) error {
	return (&probe.Reconciler{
		Client: mgr.GetClient(), New: kind, Prober: controller.Prober{Config: cfg},
		Options: probe.Options{OperatorNamespace: operatorNS},
	}).SetupWithManager
}

func runSteps(mgr ctrl.Manager, steps []setupStep) error {
	for _, step := range steps {
		if err := step.fn(mgr); err != nil {
			return fmt.Errorf("set up the %s: %w", step.name, err)
		}
	}
	return nil
}

// serveProbes answers /healthz and /readyz without a cluster, until ctx is
// cancelled. It exists so `make release-check` can smoke-test the image
// (scripts/image-smoke.sh polls a URL), which a manager that needs an API
// server could not answer.
func serveProbes(ctx context.Context, addr string) error {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux := http.NewServeMux()
	mux.Handle("/healthz", ok)
	mux.Handle("/readyz", ok)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// The shutdown goroutine waits on runCtx, which is cancelled on every way
	// out of this function, so it cannot outlive a failed ListenAndServe.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-runCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	err := srv.ListenAndServe()
	stop()
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
