/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	_ "embed"
	"flag"
	"io"
	"os"

	"github.com/operator-framework/operator-registry/pkg/image/containerdregistry"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	crfinalizer "sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/joelanford/catalogd/api/v1alpha1"
	"github.com/joelanford/catalogd/apiservers"
	"github.com/joelanford/catalogd/controllers"
	"github.com/joelanford/catalogd/finalizer"
	"github.com/joelanford/catalogd/storage"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "995b6a56.operatorframework.io",
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
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	metadataStorage, err := storage.NewBoltStorage()
	if err != nil {
		setupLog.Error(err, "unable to create metadata storage")
		os.Exit(1)
	}

	registryDir, err := os.MkdirTemp("", "registry-")
	if err != nil {
		setupLog.Error(err, "unable to create registry directory")
		os.Exit(1)
	}
	registryLog := logrus.New()
	registryLog.SetOutput(io.Discard)

	registry, err := containerdregistry.NewRegistry(
		containerdregistry.WithCacheDir(registryDir),
		containerdregistry.WithLog(logrus.NewEntry(registryLog)),
	)
	if err != nil {
		setupLog.Error(err, "unable to create containerd registry")
		os.Exit(1)
	}

	finalizers := crfinalizer.NewFinalizers()
	if err := finalizers.Register("catalogs.operatorframework.io/rm-cache", finalizer.RmCache{Storage: metadataStorage}); err != nil {
		setupLog.Error(err, "unable to register finalizers")
		os.Exit(1)
	}

	if err = (&controllers.CatalogReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Storage:    metadataStorage,
		Registry:   registry,
		Finalizers: finalizers,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Catalog")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	if err := mgr.Add(&apiservers.CatalogMetadataAPIServer{
		CertFile: "/apiserver.local.config/certificates/tls.crt",
		KeyFile:  "/apiserver.local.config/certificates/tls.key",
		Storage:  metadataStorage,
	}); err != nil {
		setupLog.Error(err, "unable to add blob api server")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
