// kumaco is the Kubernetes maintenance controller: see README.md.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"time"

	// kumaco's image is built FROM scratch: embed the time zone database of
	// the Go release (README.md, "Time zones"). ZONEINFO still replaces it.
	_ "time/tzdata"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kumacov1alpha1 "github.com/kubereboot/kumaco/api/v1alpha1"
	"github.com/kubereboot/kumaco/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kumacov1alpha1.AddToScheme(scheme))
}

// config is the command line of kumaco. See README.md, "Configuration".
type config struct {
	controllerName      string
	nodeSelector        string
	windowLabelSelector string
	conditionPrefix     string
	heartbeatInterval   time.Duration
	kubeAPIQPS          float64
	kubeAPIBurst        int

	metricsAddr     string
	metricsSecure   bool
	metricsCertPath string
	metricsCertName string
	metricsCertKey  string
	enableHTTP2     bool
	probeAddr       string
	leaderElect     bool
}

func (c *config) bind(flags *flag.FlagSet) {
	flags.StringVar(&c.controllerName, "controller-name", "kumaco",
		"Name of this controller: the name of its leader-election Lease, and the source of its events. "+
			"Controllers for different teams need different names.")
	flags.StringVar(&c.nodeSelector, "node-selector", "",
		"Label selector of the nodes this controller manages. Empty selects all nodes, which is only safe with a single controller.")
	flags.StringVar(&c.windowLabelSelector, "window-label-selector", "",
		"Label selector of the MaintenanceWindows this controller reconciles. Empty selects all windows.")
	flags.StringVar(&c.conditionPrefix, "condition-prefix", kumacov1alpha1.DefaultConditionPrefix,
		"Reserved prefix that every conditionType must start with.")
	flags.DurationVar(&c.heartbeatInterval, "heartbeat-interval", 5*time.Minute,
		"How often lastHeartbeatTime is refreshed on the node conditions kumaco owns.")
	flags.Float64Var(&c.kubeAPIQPS, "kube-api-qps", 50, "Sustained rate of requests to the Kubernetes API, per second.")
	flags.IntVar(&c.kubeAPIBurst, "kube-api-burst", 100, "Maximum burst of requests to the Kubernetes API.")

	flags.StringVar(&c.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flags.BoolVar(&c.metricsSecure, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flags.StringVar(&c.metricsCertPath, "metrics-cert-path", "", "The directory that contains the metrics server certificate.")
	flags.StringVar(&c.metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flags.StringVar(&c.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flags.BoolVar(&c.enableHTTP2, "enable-http2", false, "If set, HTTP/2 will be enabled for the metrics server.")
	flags.StringVar(&c.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flags.BoolVar(&c.leaderElect, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
}

// validate checks what the flag types cannot.
func (c *config) validate() (nodes, windows labels.Selector, err error) {
	if nodes, err = labels.Parse(c.nodeSelector); err != nil {
		return nil, nil, fmt.Errorf("--node-selector: %w", err)
	}
	if windows, err = labels.Parse(c.windowLabelSelector); err != nil {
		return nil, nil, fmt.Errorf("--window-label-selector: %w", err)
	}
	switch {
	case c.controllerName == "":
		err = fmt.Errorf("--controller-name is empty")
	case c.conditionPrefix == "":
		err = fmt.Errorf("--condition-prefix is empty: kumaco would own every condition type")
	case c.heartbeatInterval <= 0:
		err = fmt.Errorf("--heartbeat-interval must be positive, got %s", c.heartbeatInterval)
	case c.kubeAPIQPS <= 0 || c.kubeAPIBurst <= 0:
		err = fmt.Errorf("--kube-api-qps and --kube-api-burst must be positive, got %v and %d", c.kubeAPIQPS, c.kubeAPIBurst)
	}
	return nodes, windows, err
}

// bindFlags defines the command line: the flags of kumaco, and those of the
// logging backend that Kubebuilder scaffolds.
func bindFlags(flags *flag.FlagSet) (*config, *zap.Options) {
	cfg := &config{}
	cfg.bind(flags)
	zapOptions := &zap.Options{Development: true}
	zapOptions.BindFlags(flags)
	return cfg, zapOptions
}

func main() {
	cfg, zapOptions := bindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New(zap.UseFlagOptions(zapOptions))
	ctrl.SetLogger(logger)
	// client-go logs through klog: same output, same verbosity.
	klog.SetLogger(logger)
	setupLog := ctrl.Log.WithName("setup")

	if err := run(*cfg); err != nil {
		setupLog.Error(err, "Failed to run the manager")
		os.Exit(1)
	}
}

func run(cfg config) error {
	nodeSelector, windowSelector, err := cfg.validate()
	if err != nil {
		return err
	}

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	// One rate limiter for every client: the flags bound all the requests of
	// kumaco, not those of each client (README.md, "API request rate").
	restConfig.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(float32(cfg.kubeAPIQPS), cfg.kubeAPIBurst)

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Cache:                  controller.CacheOptions(nodeSelector, windowSelector),
		Metrics:                cfg.metricsOptions(),
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         cfg.leaderElect,
		LeaderElectionID:       cfg.controllerName,
	})
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}
	clientset, err := kubernetes.NewForConfigAndClient(restConfig, mgr.GetHTTPClient())
	if err != nil {
		return fmt.Errorf("creating the node client: %w", err)
	}
	err = controller.Setup(mgr, clientset.CoreV1().Nodes(), controller.Options{
		ControllerName:    cfg.controllerName,
		ConditionPrefix:   cfg.conditionPrefix,
		HeartbeatInterval: cfg.heartbeatInterval,
		Metrics:           ctrlmetrics.Registry,
	})
	if err != nil {
		return fmt.Errorf("setting up the controller: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up the health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up the ready check: %w", err)
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}

// metricsOptions configures the metrics endpoint as Kubebuilder scaffolds it.
func (c *config) metricsOptions() metricsserver.Options {
	options := metricsserver.Options{
		BindAddress:   c.metricsAddr,
		SecureServing: c.metricsSecure,
		CertDir:       c.metricsCertPath,
		CertName:      c.metricsCertName,
		KeyName:       c.metricsCertKey,
	}
	if !c.enableHTTP2 {
		// HTTP/2 is disabled by default, because of its Stream Cancellation
		// and Rapid Reset vulnerabilities (GHSA-qppj-fm5r-hxr3, GHSA-4374-p667-p6c8).
		options.TLSOpts = append(options.TLSOpts, func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} })
	}
	if c.metricsSecure {
		// Only authenticated and authorized clients can read the metrics.
		options.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	return options
}
