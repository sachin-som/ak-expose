package main

import (
	"flag"
	"time"

	// metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

func buildFromConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	// Build config from incluster-config
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func main() {
	klog.InitFlags(nil)

	var kubeconfig string
	var namespace string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to your kubeconfig file")
	flag.StringVar(&namespace, "namespace", "default", "kubernetes namespaces")
	flag.Parse()

	config, err := buildFromConfig(kubeconfig)

	if err != nil {
		klog.Fatal(err)
	}

	client, err := clientset.NewForConfig(config)

	if err != nil {
		klog.Fatal(err)
	}

	// pods, err := client.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})

	// if err != nil {
	// 	klog.Fatal(err)
	// }

	// klog.Info("pods from default namespace...")
	// for _, pod := range pods.Items {
	// 	klog.Infof("%s", pod.Name)
	// }

	informerFactory := informers.NewSharedInformerFactory(client, 10*time.Second)

	stopCh := make(chan struct{})
	controller := newController(*client, informerFactory.Apps().V1().Deployments())
	informerFactory.Start(stopCh)
	controller.run(stopCh)
}
