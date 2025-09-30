package main

import (
	"context"
	"fmt"

	"github.com/spf13/pflag"
	"github.com/wzshiming/kube-activator/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"
)

type flagpole struct {
	IP         string
	Kubeconfig string
	MasterURL  string
}

func main() {
	ctx := context.Background()

	var f flagpole
	pflag.StringVar(&f.IP, "ip", "", "The ip of the activator")
	pflag.StringVar(&f.Kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	pflag.StringVar(&f.MasterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	pflag.Parse()

	if f.IP == "" {
		pflag.Usage()
		return
	}

	klog.InfoS("start", "ip", f.IP)
	err := run(ctx, &f)
	if err != nil {
		klog.ErrorS(err, "run")
	}
	select {}
}

func run(ctx context.Context, f *flagpole) error {
	var restConfig *rest.Config
	if f.Kubeconfig == "" {
		clientConfig, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("could not get in ClusterConfig: %w", err)
		}
		if f.MasterURL != "" {
			clientConfig.Host = f.MasterURL
		}
		restConfig = clientConfig
	} else {
		clientConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: f.Kubeconfig},
			&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: f.MasterURL}}).ClientConfig()
		if err != nil {
			return fmt.Errorf("could not get Kubernetes config: %w", err)
		}
		restConfig = clientConfig
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("could not get Kubernetes clientset: %w", err)
	}

	s := server.NewServer(f.IP, clientset)
	return s.Run(ctx)

}
