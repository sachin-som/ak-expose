package main

import (
	"context"
	"fmt"
	"time"

	apiappsv1 "k8s.io/api/apps/v1"
	apicorev1 "k8s.io/api/core/v1"
	apinetv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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
		// UpdateFunc: c.handleUpdateDeployment,
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

		// Check if the deployemnt is deleted
		if errors.IsNotFound(err) {
			klog.Infof("handle delete event for dep %s\n", name)
			err := c.clientset.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{})
			if err != nil {
				klog.Infof("deleting service %s, error %s\n", name, err.Error())
				return err
			}

			err = c.clientset.NetworkingV1().Ingresses(ns).Delete(ctx, name, metav1.DeleteOptions{})
			if err != nil {
				klog.Infof("deleting ingrss %s, error %s\n", name, err.Error())
				return err
			}

			return nil
		}

		klog.Fatal("Error in getting deployment: ", err.Error())
		return err
	}

	createdsvc, err := c.createService(ctx, deployment)

	if err != nil {
		klog.Infof("Error in creating service: %s", err.Error())
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}

	if createdsvc == nil {
		return nil
	}

	klog.Info("Created service with name ", createdsvc.Name, " in namespace ", createdsvc.Namespace)

	// create ingress object
	createding, err := c.createIngress(ctx, createdsvc)

	if err != nil {
		klog.Infof("Error in creating ingress: %s", err.Error())
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}

	if createding != nil {
		klog.Infof("Created ingress with name %s in namespace %s", createding.Name, createding.Namespace)
	}
	return nil
}

func (c *controller) createService(context context.Context, deployment *apiappsv1.Deployment) (*apicorev1.Service, error) {

	// Dynamically create service ports as per pods ports
	// Extract container ports from the deployment specification (deployment.spec.template.spec.container.ports)
	// Map the above ports to the target ports
	// Create unique ports for port/map the same target port to the port
	containers := deployment.Spec.Template.Spec.Containers

	if len(containers) == 0 {
		klog.InfoS("No containers are found inside %s deployment, skipping service creation.", deployment.Name)
		return nil, nil
	}

	var servicePorts []apicorev1.ServicePort
	portIndex := 0
	for _, container := range containers {
		for _, port := range container.Ports {
			servicePorts = append(servicePorts, apicorev1.ServicePort{
				Name:       fmt.Sprintf("port-%d", portIndex),
				Protocol:   port.Protocol,
				Port:       port.ContainerPort,
				TargetPort: intstr.FromInt(int(port.ContainerPort)),
				// NodePort: "", // TODO: maybe someday take it as input
			})
			portIndex++
		}
	}

	if len(servicePorts) == 0 {
		klog.Infof("No ports found in deployment %s, skipping service creation.", deployment.Name)
		return nil, nil
	}

	// create service object
	svcObj := apicorev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployment.Name,
			Namespace: deployment.Namespace,
		},
		Spec: apicorev1.ServiceSpec{
			Selector: deploymentLabels(*deployment),
			Ports:    servicePorts,
		},
	}

	return c.clientset.CoreV1().Services(deployment.Namespace).Create(context, &svcObj, metav1.CreateOptions{})
}

func (c *controller) createIngress(context context.Context, service *apicorev1.Service) (*apinetv1.Ingress, error) {
	ingressPathType := "Prefix"
	ingObj := apinetv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name: service.Name,
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
									Path:     fmt.Sprintf("/%s", service.Name),
									PathType: (*apinetv1.PathType)(&ingressPathType),
									Backend: apinetv1.IngressBackend{
										Service: &apinetv1.IngressServiceBackend{
											Name: service.Name,
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

	return c.clientset.NetworkingV1().Ingresses(service.Namespace).Create(context, &ingObj, metav1.CreateOptions{})
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

// func (c *controller) handleUpdateDeployment(oldObj, newObj interface{}) {
// 	klog.Info("Added object to workqueue on update deployment")
// 	c.queue.Add(newObj)
// }
