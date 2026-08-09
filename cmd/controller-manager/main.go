package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	sandboxcontroller "github.com/AgentNaut/SandboxFleet/internal/controller"
	"github.com/AgentNaut/SandboxFleet/internal/scheduler"
	"github.com/AgentNaut/SandboxFleet/internal/worker/httpapi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	var (
		metricsAddress    = flag.String("metrics-bind-address", ":8080", "Address for metrics.")
		probeAddress      = flag.String("health-probe-bind-address", ":8081", "Address for health probes.")
		leaderElection    = flag.Bool("leader-elect", true, "Enable leader election.")
		workerImage       = flag.String("worker-image", "sandboxfleet-worker-gvisor:latest", "Worker image.")
		workerPort        = flag.Int("worker-port", 8090, "Worker HTTP API port.")
		workerHTTPTimeout = flag.Duration("worker-http-timeout", 5*time.Minute, "Timeout for Worker HTTP calls.")
	)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true})))

	scheme := runtime.NewScheme()
	must(clientgoscheme.AddToScheme(scheme))
	must(appsv1.AddToScheme(scheme))
	must(corev1.AddToScheme(scheme))
	must(sandboxv1alpha1.AddToScheme(scheme))

	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddress},
		HealthProbeBindAddress: *probeAddress,
		LeaderElection:         *leaderElection,
		LeaderElectionID:       "sandboxfleet-controller.sandboxfleet.io",
	})
	must(err)

	slotScheduler := scheduler.New(scheduler.BinPackStrategy{})
	workerClient := httpapi.NewClient(&http.Client{Timeout: *workerHTTPTimeout})
	endpointResolver := sandboxcontroller.PodIPResolver{Port: int32(*workerPort)}
	must((&sandboxcontroller.PoolReconciler{
		Client:           manager.GetClient(),
		Scheme:           scheme,
		Scheduler:        slotScheduler,
		WorkerClient:     workerClient,
		EndpointResolver: endpointResolver,
		WorkerPort:       int32(*workerPort),
		WorkloadBuilder: sandboxcontroller.StatefulSetBuilder{
			DefaultImage: *workerImage,
			Port:         int32(*workerPort),
		},
	}).SetupWithManager(manager))
	must((&sandboxcontroller.SandboxReconciler{
		Client:           manager.GetClient(),
		Scheme:           scheme,
		Scheduler:        slotScheduler,
		WorkerClient:     workerClient,
		EndpointResolver: endpointResolver,
	}).SetupWithManager(manager))
	must((&sandboxcontroller.SnapshotReconciler{
		Client:           manager.GetClient(),
		Scheme:           scheme,
		WorkerClient:     workerClient,
		EndpointResolver: endpointResolver,
	}).SetupWithManager(manager))

	must(manager.AddHealthzCheck("healthz", healthz.Ping))
	must(manager.AddReadyzCheck("readyz", healthz.Ping))
	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "controller manager stopped")
		os.Exit(1)
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
