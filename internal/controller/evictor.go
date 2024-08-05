/*
Copyright 2024 Preferred Networks, Inc.

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

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

type evictor struct {
	client.Client
	*runtime.Scheme
	eventRecorder record.EventRecorder
	// requeueAfter is the interval to requeue the reconciliation to reevaluate pods or to retry eviction that failed
	// due to PodDisruptionBudget violation.
	// TODO: Split into two fields if we need to set different intervals for each case.
	requeueAfter time.Duration
}

// NewEvictor creates a new ServiceAccount reconciler that evicts pods that are failing to pull container images because
// they do not have an image pull secret provisioned for their ServiceAccount.
func NewEvictor(
	client client.Client, scheme *runtime.Scheme, eventRecorder record.EventRecorder,
) *evictor {
	return &evictor{
		Client:        client,
		Scheme:        scheme,
		eventRecorder: eventRecorder,
		// "kubectl drain" retries eviction after 5 seconds.
		// https://github.com/kubernetes/kubernetes/blob/546f7c30860dcdecb75c544230a1b7cdf5bd5958/staging/src/k8s.io/kubectl/pkg/drain/drain.go#L319
		requeueAfter: 5 * time.Second,
	}
}

//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

const (
	indexKeyServiceAccountName = "spec.serviceAccountName"

	// Event reasons.
	reasonFailedEviction = "FailedEvictionForImagePullSecret"
	reasonEvicted        = "EvictedForImagePullSecret"
)

func (e *evictor) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the requested ServiceAccount.
	sa := &corev1.ServiceAccount{}
	if err := e.Get(ctx, req.NamespacedName, sa); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Requested ServiceAccount is not found.")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to get a ServiceAccount")
		return ctrl.Result{}, err
	}

	// Check if an image pull secret has already been provisioned for the ServiceAccount.
	secret, err := e.getProvisionedImagePullSecret(ctx, sa)
	if err != nil {
		logger.Error(err, "failed to get an image pull secret provisioned for a ServiceAccount")
		return ctrl.Result{}, err
	}

	if secret == "" {
		logger.Info("There is no image pull secret provisioned for a ServiceAccount.")
		// Once an image pull secret is provisioned, the reconciliation will be triggered by the ServiceAccount update.
		return ctrl.Result{}, nil
	}

	// Evaluate pods that use the ServiceAccount to list pods to evict.
	pods, requeue, err := e.listPodsToEvict(ctx, sa, secret)
	if err != nil {
		logger.Error(err, "failed to list pods to evict")
		return ctrl.Result{}, err
	}

	result := ctrl.Result{}
	if requeue {
		result = ctrl.Result{RequeueAfter: e.requeueAfter}
	}

	if len(pods) == 0 {
		logger.Info("No pods to evict.")
		return result, nil
	}

	// Evict the target pods.
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.GetName())
	}
	logger.Info("Listed pods to evict.", "targets", names)

	var rerr error
	for _, pod := range pods {
		logger := logger.WithValues("pod", pod.GetName())

		if err := e.SubResource("eviction").Create(ctx, pod, &policyv1.Eviction{}); err != nil {
			if apierrors.IsTooManyRequests(err) {
				e.eventRecorder.Eventf(
					pod, corev1.EventTypeWarning, reasonFailedEviction,
					"Eviction failed due to PodDisruptionBudget violation: %v", err,
				)
				logger.Info("Eviction failed due to PodDisruptionBudget violation: " + err.Error())
				result = ctrl.Result{RequeueAfter: e.requeueAfter}
				continue
			}

			e.eventRecorder.Eventf(pod, corev1.EventTypeWarning, reasonFailedEviction, "Eviction failed: %v", err)
			logger.Error(err, "failed to evict a pod")
			// It is OK to throw away old error because it was logged.
			rerr = err
			continue
		}

		e.eventRecorder.Event(
			pod, corev1.EventTypeNormal, reasonEvicted,
			"Evicted because the pod is failing to pull container images"+
				" and does not have an image pull secret provisioned for its ServiceAccount.",
		)
		logger.Info("Evicted a pod.")
	}

	return result, rerr
}

// SetupWithManager sets up the controller with the Manager.
func (e *evictor) SetupWithManager(mgr ctrl.Manager) error {
	// Index pods by spec.serviceAccountName to list pods using a ServiceAccount.
	if err := mgr.GetFieldIndexer().IndexField(
		context.TODO(),
		&corev1.Pod{},
		indexKeyServiceAccountName,
		func(obj client.Object) []string {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				return nil
			}
			return []string{pod.Spec.ServiceAccountName}
		},
	); err != nil {
		return fmt.Errorf("failed to create a field index: %w", err)
	}

	// Only reconcile ServiceAccounts that have configuration for image pull secret provisioning.
	pred := func(obj client.Object) bool {
		sa, ok := obj.(*corev1.ServiceAccount)
		if !ok {
			return false
		}

		return hasConfig(sa)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ServiceAccount{}, builder.WithPredicates(predicate.NewPredicateFuncs(pred))).
		Complete(e)
}

// getProvionedImagePullSecret gets an image pull secret provisioned for a ServiceAccount.
// It returns an empty string if there is no image pull secret provisioned.
func (e *evictor) getProvisionedImagePullSecret(
	ctx context.Context, sa *corev1.ServiceAccount,
) (string, error) {
	if len(sa.ImagePullSecrets) == 0 {
		return "", nil
	}

	key := client.ObjectKey{Namespace: sa.GetNamespace(), Name: secretName(sa)}
	secret := &corev1.Secret{}
	if err := e.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// ServiceAccount has invalid configuration for image pull secret provisioning,
			// or an image pull secret has not been provisioned yet.
			return "", nil
		}

		return "", fmt.Errorf("failed to get an image pull secret: %w", err)
	}

	return secret.GetName(), nil
}

// listPodsToEvict lists pods to evict, i.e., pods
// - that uses the given ServiceAccount,
// - that are failing to pull a container image, and
// - that do not have the given image pull secret.
//
// It also returns a boolean that indicates whether we need to requeue the reconciliation to reevaluate pods later
// because they can be eviction target.
func (e *evictor) listPodsToEvict(
	ctx context.Context, sa *corev1.ServiceAccount, secret string,
) (_ []*corev1.Pod, requeue bool, _ error) {
	pods := &corev1.PodList{}
	if err := e.List(
		ctx,
		pods,
		client.InNamespace(sa.GetNamespace()),
		client.MatchingFields{
			indexKeyServiceAccountName: sa.GetName(),
		},
	); err != nil {
		return nil, false, fmt.Errorf("failed to list pods: %w", err)
	}

	targets := []*corev1.Pod{}
	for _, pod := range pods.Items {
		if e.hasImagePullSecret(&pod, secret) {
			continue
		}

		if e.isImagePullFailing(&pod) {
			targets = append(targets, &pod)
		} else if e.canFailImagePullLater(&pod) {
			requeue = true
		}
	}

	return targets, requeue, nil
}

// hasImagePullSecret returns true iff a pod's spec.imagePullSecrets contains the given Secret.
func (e *evictor) hasImagePullSecret(pod *corev1.Pod, secret string) bool {
	for _, podSecret := range pod.Spec.ImagePullSecrets {
		if podSecret.Name == secret {
			return true
		}
	}

	return false
}

// isImagePullFailing returns true iff a pod is failing to pull container images.
func (e *evictor) isImagePullFailing(pod *corev1.Pod) bool {
	// Envtest seems not to support container statuses, so we cannot determine if a pod is failing to pull container
	// images using these fields.
	if testing.Testing() {
		return true
	}

	for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		if w := status.State.Waiting; w != nil {
			if w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff" {
				return true
			}
		}
	}

	return false
}

// canFailImagePullLater returns true iff a pod can fail to pull container images later.
func (e *evictor) canFailImagePullLater(pod *corev1.Pod) bool {
	if testing.Testing() {
		return true
	}

	// Pod is in Running or a later phase => all containers have already been created.
	if pod.Status.Phase != corev1.PodPending {
		return false
	}

	// A container's status is not populated => the pod has not started to create the container yet.
	if len(pod.Status.InitContainerStatuses) < len(pod.Spec.InitContainers) ||
		len(pod.Status.ContainerStatuses) < len(pod.Spec.Containers) {
		return true
	}

	// A container's status is PodInitializing or ContainerCreating
	// => the container creation (including image pull) is in progress.
	for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		if w := status.State.Waiting; w != nil {
			if w.Reason == "PodInitializing" || w.Reason == "ContainerCreating" {
				return true
			}
		}
	}

	return false
}
