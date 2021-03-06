package main

import (
	"flag"
	"log"
	"os"
	"time"

	"github.com/openfaas/faas-netes/k8s"
	providertypes "github.com/openfaas/faas-provider/types"

	clientset "github.com/openfaas-incubator/openfaas-operator/pkg/client/clientset/versioned"
	informers "github.com/openfaas-incubator/openfaas-operator/pkg/client/informers/externalversions"
	"github.com/openfaas-incubator/openfaas-operator/pkg/controller"
	"github.com/openfaas-incubator/openfaas-operator/pkg/server"
	"github.com/openfaas-incubator/openfaas-operator/pkg/signals"
	"github.com/openfaas-incubator/openfaas-operator/pkg/version"
	"github.com/openfaas/faas-netes/types"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	glog "k8s.io/klog"

	// required to authenticate against GKE clusters
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

var (
	masterURL  string
	kubeconfig string
)

var pullPolicyOptions = map[string]bool{
	"Always":       true,
	"IfNotPresent": true,
	"Never":        true,
}

func init() {
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
}

func main() {
	flag.Set("logtostderr", "true")
	flag.Parse()

	setupLogging()

	sha, release := version.GetReleaseInfo()
	glog.Infof("Starting OpenFaaS controller version: %s commit: %s", release, sha)

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()

	cfg, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		glog.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		glog.Fatalf("Error building Kubernetes clientset: %s", err.Error())
	}

	faasClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		glog.Fatalf("Error building OpenFaaS clientset: %s", err.Error())
	}

	readConfig := types.ReadConfig{}
	osEnv := providertypes.OsEnv{}
	config, err := readConfig.Read(osEnv)

	if err != nil {
		panic(err)
	}

	deployConfig := k8s.DeploymentConfig{
		RuntimeHTTPPort: 8080,
		HTTPProbe:       config.HTTPProbe,
		SetNonRootUser:  config.SetNonRootUser,
		ReadinessProbe: &k8s.ProbeConfig{
			InitialDelaySeconds: int32(config.ReadinessProbeInitialDelaySeconds),
			TimeoutSeconds:      int32(config.ReadinessProbeTimeoutSeconds),
			PeriodSeconds:       int32(config.ReadinessProbePeriodSeconds),
		},
		LivenessProbe: &k8s.ProbeConfig{
			InitialDelaySeconds: int32(config.LivenessProbeInitialDelaySeconds),
			TimeoutSeconds:      int32(config.LivenessProbeTimeoutSeconds),
			PeriodSeconds:       int32(config.LivenessProbePeriodSeconds),
		},
		ImagePullPolicy: config.ImagePullPolicy,
	}

	factory := controller.NewFunctionFactory(kubeClient, deployConfig)

	functionNamespace := "openfaas-fn"
	if namespace, exists := os.LookupEnv("function_namespace"); exists {
		functionNamespace = namespace
	}

	if !pullPolicyOptions[config.ImagePullPolicy] {
		glog.Fatalf("Invalid image_pull_policy configured: %s", config.ImagePullPolicy)
	}

	defaultResync := time.Second * 5

	kubeInformerOpt := kubeinformers.WithNamespace(functionNamespace)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(kubeClient, defaultResync, kubeInformerOpt)

	faasInformerOpt := informers.WithNamespace(functionNamespace)
	faasInformerFactory := informers.NewSharedInformerFactoryWithOptions(faasClient, defaultResync, faasInformerOpt)

	endpointsInformer := kubeInformerFactory.Core().V1().Endpoints()
	deploymentInformer := kubeInformerFactory.Apps().V1().Deployments()

	log.Printf("Waiting for cache sync in main")
	kubeInformerFactory.WaitForCacheSync(stopCh)
	log.Printf("Cache sync done")

	ctrl := controller.NewController(
		kubeClient,
		faasClient,
		kubeInformerFactory,
		faasInformerFactory,
		factory,
	)

	srv := server.New(faasClient, kubeClient, endpointsInformer, deploymentInformer)

	go faasInformerFactory.Start(stopCh)
	go kubeInformerFactory.Start(stopCh)

	go srv.Start()
	if err = ctrl.Run(1, stopCh); err != nil {
		glog.Fatalf("Error running controller: %s", err.Error())
	}
}

func setupLogging() {
	klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
	glog.InitFlags(klogFlags)

	// Sync the glog and klog flags.
	flag.CommandLine.VisitAll(func(f1 *flag.Flag) {
		f2 := klogFlags.Lookup(f1.Name)
		if f2 != nil {
			value := f1.Value.String()
			f2.Value.Set(value)
		}
	})
}
