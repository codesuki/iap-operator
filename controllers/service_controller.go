/*
Copyright 2021.

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

package controllers

import (
	"context"
	"fmt"
	"time"

	iap "cloud.google.com/go/iap/apiv1"
	iamv1 "google.golang.org/genproto/googleapis/iam/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/ingress-gce/pkg/annotations"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	iapv1alpha1 "github.com/codesuki/iap-operator/api/v1alpha1"
)

const (
	iapConfigAnnotation = "iap.codesuki.github.io/config"
)

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=services/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Service object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.9.2/pkg/reconcile
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var service corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &service); err != nil {
		// does this mean it's deleted? how do we find out if that is supposed to have happened?
		logger.Error(err, "unable to fetch %s/%s", req.Namespace, req.Name)
		return ctrl.Result{}, err
	}

	// check if it has backends
	negStatus, ok, err := annotations.FromService(&service).NEGStatus()
	if err != nil || !ok {
		// doesn't have backends so we cannot do anything
		return ctrl.Result{}, nil
	}

	// extract the backends
	var backends []string
	for _, backend := range negStatus.NetworkEndpointGroups {
		backends = append(backends, backend)
	}

	// if it has no annotation: check that perms are empty
	var members []string
	configName, ok := service.Annotations[iapConfigAnnotation]
	if ok {
		var config iapv1alpha1.Config
		if err := r.Get(ctx, client.ObjectKey{Namespace: service.Namespace, Name: configName}, &config); err == nil {
			members = config.Spec.Members
			logger.Info("found config", "name", configName)
		}
	}

	// sync each backend that has iap enabled <- not sure if we care, we won't apply the IAPConfig in the first place
	// if the config does not exist check that perms are empty
	// if config exists: apply permissions
	logger.Info("syncing policies for", "backends", backends)
	err = syncBackendIAMPolicies(backends, members)

	// TODO:
	// - trigger on config add/update/delete
	// -- search services that refer to it and trigger
	// - take watched namespaces as parameters
	// - take gcp project name as parameter
	// - make timeout configurable
	// - consider if this should act on all services or should we explicitly enable it?
	// -- in this case we run into weird conditions, what if we remove the managed flag before deleting the config?
	// --- then the config will stay applied
	return ctrl.Result{}, err
}

func syncBackendIAMPolicies(backends []string, members []string) error {
	client, err := newIAPClient()
	if err != nil {
		return err
	}
	defer client.Close()

	for i := range backends {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// TODO: should we only set if there is a difference? either way we always make a call
		err = client.setBackendIAMPolicy(ctx, backends[i], members)
	}
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(&source.Kind{Type: &iapv1alpha1.Config{}},
			handler.Funcs{
				CreateFunc: func(e event.CreateEvent, q workqueue.RateLimitingInterface) {
					client := client.NewNamespacedClient(r.Client, e.Object.GetNamespace())
					e.Object.GetName()
					// search services in namespace that have the annotation
					var serviceList corev1.ServiceList
					client.List(context.Background(), &serviceList)
					for i := range serviceList.Items {
						if serviceList.Items[i].Annotations[iapConfigAnnotation] == e.Object.GetName() {
							q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
								Name:      serviceList.Items[i].GetName(),
								Namespace: serviceList.Items[i].GetNamespace(),
							}})
							fmt.Println(
								"queueing for reconcilation",
								serviceList.Items[i].GetNamespace(),
								serviceList.Items[i].GetName(),
							)
						}
					}
				},
				UpdateFunc: func(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
					client := client.NewNamespacedClient(r.Client, e.ObjectOld.GetNamespace())
					e.ObjectOld.GetName()
					// search services in namespace that have the annotation
					var serviceList corev1.ServiceList
					client.List(context.Background(), &serviceList)
					for i := range serviceList.Items {
						if serviceList.Items[i].Annotations[iapConfigAnnotation] == e.ObjectOld.GetName() {
							q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
								Name:      serviceList.Items[i].GetName(),
								Namespace: serviceList.Items[i].GetNamespace(),
							}})
							fmt.Println(
								"queueing for reconcilation",
								serviceList.Items[i].GetNamespace(),
								serviceList.Items[i].GetName(),
							)
						}
					}
				},
				DeleteFunc: func(e event.DeleteEvent, q workqueue.RateLimitingInterface) {
					client := client.NewNamespacedClient(r.Client, e.Object.GetNamespace())
					e.Object.GetName()
					// search services in namespace that have the annotation
					var serviceList corev1.ServiceList
					client.List(context.Background(), &serviceList)
					for i := range serviceList.Items {
						if serviceList.Items[i].Annotations[iapConfigAnnotation] == e.Object.GetName() {
							q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
								Name:      serviceList.Items[i].GetName(),
								Namespace: serviceList.Items[i].GetNamespace(),
							}})
							fmt.Println(
								"queueing for reconcilation",
								serviceList.Items[i].GetNamespace(),
								serviceList.Items[i].GetName(),
							)
						}
					}
				},
			},
		).
		Complete(r)
}

type iapClient struct {
	client *iap.IdentityAwareProxyAdminClient
}

func newIAPClient() (*iapClient, error) {
	c, err := iap.NewIdentityAwareProxyAdminClient(context.Background())
	if err != nil {
		return nil, err
	}
	return &iapClient{client: c}, nil
}

func (c *iapClient) setIAMPolicy(ctx context.Context, req *iamv1.SetIamPolicyRequest) error {
	_, err := c.client.SetIamPolicy(ctx, req)
	return err
}

func (c *iapClient) setBackendIAMPolicy(ctx context.Context, backend string, members []string) error {
	resource := fmt.Sprintf("projects/%s/iap_web/compute/services/%s", project, backend)
	req := &iamv1.SetIamPolicyRequest{
		// See https://pkg.go.dev/google.golang.org/genproto/googleapis/iam/v1#SetIamPolicyRequest.
		Resource: resource,
		Policy: &iamv1.Policy{
			Version: 1,
			Bindings: []*iamv1.Binding{
				{
					Role:    "roles/iap.httpsResourceAccessor",
					Members: members,
				},
			},
			// Etag: etag, // we don't set this because we are the authority
		},
	}
	return c.setIAMPolicy(ctx, req)
}

func (c *iapClient) Close() {
	c.client.Close()
}
