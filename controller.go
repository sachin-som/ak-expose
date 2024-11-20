package main

import (
	"context"
	"time"

	apiappsv1 "k8s.io/api/apps/v1"
	apicorev1 "k8s.io/api/core/v1"
	apinetv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

type controller struct {
	clientset             kubernetes.Clientset
	deploymentLister      appslisters.DeploymentLister
	deploymentCacheSynced cache.InformerSynced
	queue                 workqueue.RateLimitingInterface
}

func newController(clientset kubernetes.Clientset, deploymentInformer appsinformers.DeploymentInformer) *controller {
	c := &controller{
		clientset:             clientset,
		deploymentLister:      deploymentInformer.Lister(),
		deploymentCacheSynced: deploymentInformer.Informer().HasSynced,
		queue:                 workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ak-expose"),
	}

	// register event handlers
	deploymentInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.handleAddDeployment,
		DeleteFunc: c.handleDeleteDeployment,
	})
	return c
}

func (c *controller) run(stopCh <-chan struct{}) {
	klog.Info("Starting controller...")
	cache.WaitForNamedCacheSync("ak-expose", stopCh, c.deploymentCacheSynced)
	go wait.Until(c.worker, 1*time.Second, stopCh)

	// This will keep the run method in running state
	<-stopCh
}

func (c *controller) worker() {
	klog.Info("Informer synced. Started worker execution...")

	// Keep the loop running it receives `true`
	for c.processItem() {

	}
}

func (c *controller) processItem() bool {
	// klog.Info("processing items from workqueue...")
	item, shutdown := c.queue.Get()

	if shutdown {
		return false
	}
	c.queue.Forget(item)
	key, err := cache.MetaNamespaceKeyFunc(item)

	if err != nil {
		klog.Fatal("Error while getting namespace key: ", err.Error())
	}

	ns, name, err := cache.SplitMetaNamespaceKey(key)

	if err != nil {
		klog.Fatal("Error while splitting namespace key: ", err.Error())

		// TODO: re-try with the same object
		return false
	}

	err = c.syncDeployment(ns, name)
	if err != nil {
		klog.Fatal("Error in syncying deployment: ", err.Error())
		return false
	}
	return true
}

func (c *controller) syncDeployment(ns, name string) error {
	ctx := context.Background()

	deployment, err := c.deploymentLister.Deployments(ns).Get(name)
	if err != nil {
		klog.Fatal("Error in getting deployment: ", err.Error())
		return err
	}

	// create service object
	svc := apicorev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployment.Name,
			Namespace: ns,
		},
		Spec: apicorev1.ServiceSpec{
			Selector: deploymentLabels(*deployment),
			Ports: []apicorev1.ServicePort{
				{
					Name: "http",
					Port: 80, // TODO: figure out name and port from deployment
				},
			},
		},
	}
	createdsvc, err := c.clientset.CoreV1().Services(ns).Create(ctx, &svc, metav1.CreateOptions{})

	if err != nil {
		klog.Fatal("Error while creating service: ", err.Error())
		return err
	}

	klog.Info("Created service with name ", deployment.Name, " in namespace ", deployment.Namespace)

	// create ingress object
	ingressPathType := "Prefix"
	ing := apinetv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: createdsvc.Name,
			Annotations: map[string]string{
				"nginx.ingress.kubernetes.io/rewrite-target": "/",
			},
		},
		Spec: apinetv1.IngressSpec{
			Rules: []apinetv1.IngressRule{
				{
					IngressRuleValue: apinetv1.IngressRuleValue{
						HTTP: &apinetv1.HTTPIngressRuleValue{
							Paths: []apinetv1.HTTPIngressPath{
								{
									Path:     createdsvc.Name,
									PathType: (*apinetv1.PathType)(&ingressPathType),
									Backend: apinetv1.IngressBackend{
										Service: &apinetv1.IngressServiceBackend{
											Name: createdsvc.Name,
											Port: apinetv1.ServiceBackendPort{
												Number: 80, // TODO: Make it dynamic
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	_, err = c.clientset.NetworkingV1().Ingresses(createdsvc.Namespace).Create(ctx, &ing, metav1.CreateOptions{})

	if err != nil {
		klog.Fatal("Error in creating ingress: ", err.Error())
		return err
	}

	return nil
}

func deploymentLabels(deployment apiappsv1.Deployment) map[string]string {
	return deployment.Spec.Template.Labels
}

func (c *controller) handleAddDeployment(obj interface{}) {
	klog.Info("Added object to workqueue on add deployment")
	c.queue.Add(obj)
}

func (c *controller) handleDeleteDeployment(obj interface{}) {
	klog.Info("Added object to workqueue on delete deployment")
	c.queue.Add(obj)
}
