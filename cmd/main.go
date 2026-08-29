// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	computev1alpha "go.datum.net/compute/api/v1alpha"

	"go.datum.net/kata-provider/internal/config"
	"go.datum.net/kata-provider/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	codecs   = serializer.NewCodecFactory(scheme, serializer.EnableStrict)
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(computev1alpha.AddToScheme(scheme))

	utilruntime.Must(config.AddToScheme(scheme))
	utilruntime.Must(config.RegisterDefaults(scheme))

	// +kubebuilder:scaffold:scheme
}

func main() {
	var enableLeaderElection bool
	var leaderElectionNamespace string
	var probeAddr string
	var serverConfigFile string

	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionNamespace, "leader-elect-namespace", "", "The namespace to use for leader election.")
	flag.StringVar(&serverConfigFile, "server-config", "", "path to the server config file")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)

	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	var serverConfig config.KataProvider
	if len(serverConfigFile) > 0 {
		configData, err := os.ReadFile(serverConfigFile)
		if err != nil {
			setupLog.Error(fmt.Errorf("unable to read server config from %q", serverConfigFile), "")
			os.Exit(1)
		}
		if err := runtime.DecodeInto(codecs.UniversalDecoder(), configData, &serverConfig); err != nil {
			setupLog.Error(err, "unable to decode server config")
			os.Exit(1)
		}
	} else {
		// Decoding is what normally applies the declared defaults, so a
		// deployment that supplies no config file still gets them.
		scheme.Default(&serverConfig)
	}

	setupLog.Info("server config", "config", serverConfig)

	cfg := ctrl.GetConfigOrDie()
	ctx := ctrl.SetupSignalHandler()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		// The cache is scoped to the runtime class this provider serves. It is
		// not an optimization: see controller.CacheOptions.
		Cache:                   controller.CacheOptions(),
		Metrics:                 serverConfig.MetricsServer.Options(ctx, nil),
		WebhookServer:           webhook.NewServer(serverConfig.WebhookServer.Options(ctx, nil)),
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        "b9c2e4a7.datumapis.com",
		LeaderElectionNamespace: leaderElectionNamespace,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.InstanceReconciler{
		Config: &serverConfig,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "InstanceReconciler")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager",
		"runtimeClass", computev1alpha.RuntimeClassGeneralPurpose,
		"runtimeHandler", serverConfig.DownstreamResourceManagement.RuntimeHandler,
	)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
