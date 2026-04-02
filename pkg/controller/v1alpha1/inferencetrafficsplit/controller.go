/*
Copyright 2024 The KServe Authors.

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

// +kubebuilder:rbac:groups=serving.kserve.io,resources=inferencetrafficsplits;inferencetrafficsplits/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.kserve.io,resources=inferencetrafficsplits/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
package inferencetrafficsplit

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	"github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	"github.com/kserve/kserve/pkg/constants"
)

var log = ctrl.Log.WithName("InferenceTrafficSplitController")

// InferenceTrafficSplitReconciler reconciles an InferenceTrafficSplit object
type InferenceTrafficSplitReconciler struct {
	client.Client
	Log           logr.Logger
	Scheme        *runtime.Scheme
	IngressConfig *v1beta1.IngressConfig
}

const trafficSplitFinalizer = "inferencetrafficsplit.serving.kserve.io/finalizer"

func (r *InferenceTrafficSplitReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log.Info("Reconciling InferenceTrafficSplit", "name", req.Name, "namespace", req.Namespace)

	// Fetch the InferenceTrafficSplit
	its := &v1alpha1.InferenceTrafficSplit{}
	if err := r.Get(ctx, req.NamespacedName, its); err != nil {
		if apierr.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion: remove labels from referenced ISVCs
	if !its.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(its, trafficSplitFinalizer) {
			for _, backend := range its.Spec.Backends {
				if err := r.removeTrafficSplitLabel(ctx, backend.InferenceServiceRef, its); err != nil {
					return ctrl.Result{}, err
				}
			}
			controllerutil.RemoveFinalizer(its, trafficSplitFinalizer)
			if err := r.Update(ctx, its); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is set
	if !controllerutil.ContainsFinalizer(its, trafficSplitFinalizer) {
		controllerutil.AddFinalizer(its, trafficSplitFinalizer)
		if err := r.Update(ctx, its); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Build weighted backends from referenced ISVCs
	var backends []gwapiv1.HTTPBackendRef
	allReady := true
	for _, backend := range its.Spec.Backends {
		isvc := &v1beta1.InferenceService{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      backend.InferenceServiceRef,
			Namespace: its.Namespace,
		}, isvc); err != nil {
			if apierr.IsNotFound(err) {
				log.Info("Referenced ISVC not found", "isvc", backend.InferenceServiceRef)
				allReady = false
				continue
			}
			return ctrl.Result{}, err
		}

		// Label the ISVC so the ISVC ingress reconciler knows to skip individual HTTPRoutes
		if err := r.ensureTrafficSplitLabel(ctx, isvc, its); err != nil {
			return ctrl.Result{}, err
		}

		if !isvc.Status.IsConditionReady(v1beta1.PredictorReady) {
			log.Info("Referenced ISVC predictor not ready", "isvc", backend.InferenceServiceRef)
			allReady = false
			continue
		}

		predictorName := constants.PredictorServiceName(isvc.Name)
		weight := backend.Weight
		backends = append(backends, gwapiv1.HTTPBackendRef{
			BackendRef: gwapiv1.BackendRef{
				BackendObjectReference: gwapiv1.BackendObjectReference{
					Kind:      ptr.To(gwapiv1.Kind(constants.ServiceKind)),
					Name:      gwapiv1.ObjectName(predictorName),
					Namespace: (*gwapiv1.Namespace)(&its.Namespace),
					Port:      ptr.To(gwapiv1.PortNumber(constants.CommonDefaultHttpPort)),
				},
				Weight: &weight,
			},
		})
	}

	if len(backends) == 0 {
		r.setStatus(its, false, "NoReadyBackends", "No referenced InferenceServices are ready")
		if err := r.Status().Update(ctx, its); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Generate the shared hostname
	host := fmt.Sprintf("%s.%s", its.Name, r.IngressConfig.IngressDomain)

	// Build the HTTPRoute
	routeMatch := []gwapiv1.HTTPRouteMatch{
		{
			Path: &gwapiv1.HTTPPathMatch{
				Type:  ptr.To(gwapiv1.PathMatchRegularExpression),
				Value: ptr.To(constants.FallbackPrefix()),
			},
		},
	}

	gatewaySlice := strings.Split(r.IngressConfig.KserveIngressGateway, "/")
	desired := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      its.Name,
			Namespace: its.Namespace,
		},
		Spec: gwapiv1.HTTPRouteSpec{
			Hostnames: []gwapiv1.Hostname{gwapiv1.Hostname(host)},
			Rules: []gwapiv1.HTTPRouteRule{
				{
					Matches:     routeMatch,
					BackendRefs: backends,
				},
			},
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{
					{
						Group:     (*gwapiv1.Group)(&gwapiv1.GroupVersion.Group),
						Kind:      (*gwapiv1.Kind)(ptr.To(constants.GatewayKind)),
						Namespace: (*gwapiv1.Namespace)(&gatewaySlice[0]),
						Name:      gwapiv1.ObjectName(gatewaySlice[1]),
					},
				},
			},
		},
	}

	// The InferenceTrafficSplit owns the HTTPRoute — clean ownership
	if err := controllerutil.SetControllerReference(its, desired, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Create or update the HTTPRoute
	existing := &gwapiv1.HTTPRoute{}
	err := r.Get(ctx, types.NamespacedName{Name: its.Name, Namespace: its.Namespace}, existing)
	if err != nil && apierr.IsNotFound(err) {
		log.Info("Creating HTTPRoute for InferenceTrafficSplit", "name", its.Name)
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, err
		}
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		desired.ResourceVersion = existing.ResourceVersion
		if err := r.Update(ctx, desired); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Update status
	readyMsg := "All backends ready"
	if !allReady {
		readyMsg = "Some backends not yet ready"
	}
	r.setStatus(its, true, "HTTPRouteReady", readyMsg)
	its.Status.URL = fmt.Sprintf("http://%s", host)
	if err := r.Status().Update(ctx, its); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *InferenceTrafficSplitReconciler) setStatus(its *v1alpha1.InferenceTrafficSplit, ready bool, reason, message string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}

	found := false
	for i, c := range its.Status.Conditions {
		if c.Type == "Ready" {
			its.Status.Conditions[i].Status = status
			its.Status.Conditions[i].Reason = reason
			its.Status.Conditions[i].Message = message
			its.Status.Conditions[i].LastTransitionTime = metav1.Now()
			found = true
			break
		}
	}
	if !found {
		its.Status.Conditions = append(its.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             status,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		})
	}
}

func (r *InferenceTrafficSplitReconciler) ensureTrafficSplitLabel(ctx context.Context, isvc *v1beta1.InferenceService, its *v1alpha1.InferenceTrafficSplit) error {
	if isvc.Labels == nil {
		isvc.Labels = make(map[string]string)
	}
	if isvc.Labels[constants.TrafficSplitLabel] == its.Name {
		return nil
	}
	isvc.Labels[constants.TrafficSplitLabel] = its.Name
	return r.Update(ctx, isvc)
}

func (r *InferenceTrafficSplitReconciler) removeTrafficSplitLabel(ctx context.Context, isvcName string, its *v1alpha1.InferenceTrafficSplit) error {
	isvc := &v1beta1.InferenceService{}
	if err := r.Get(ctx, types.NamespacedName{Name: isvcName, Namespace: its.Namespace}, isvc); err != nil {
		if apierr.IsNotFound(err) {
			return nil
		}
		return err
	}
	if isvc.Labels != nil && isvc.Labels[constants.TrafficSplitLabel] == its.Name {
		delete(isvc.Labels, constants.TrafficSplitLabel)
		return r.Update(ctx, isvc)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
// Watches InferenceTrafficSplit resources and also watches InferenceService changes
// so that when a referenced ISVC becomes ready, the InferenceTrafficSplit is re-reconciled.
func (r *InferenceTrafficSplitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.InferenceTrafficSplit{}).
		Owns(&gwapiv1.HTTPRoute{}).
		Watches(&v1beta1.InferenceService{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				// When an ISVC changes, find all InferenceTrafficSplits that reference it
				isvc := obj.(*v1beta1.InferenceService)
				itsList := &v1alpha1.InferenceTrafficSplitList{}
				if err := mgr.GetClient().List(ctx, itsList, client.InNamespace(isvc.Namespace)); err != nil {
					return nil
				}
				var requests []reconcile.Request
				for _, its := range itsList.Items {
					for _, backend := range its.Spec.Backends {
						if backend.InferenceServiceRef == isvc.Name {
							requests = append(requests, reconcile.Request{
								NamespacedName: types.NamespacedName{
									Name:      its.Name,
									Namespace: its.Namespace,
								},
							})
							break
						}
					}
				}
				return requests
			},
		)).
		Complete(r)
}
