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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type serviceAccountReconciler struct {
	client.Client
	*runtime.Scheme
	eventRecorder record.EventRecorder
	aws           aws
	google        google
	// Grace period for refreshing image pull secrets before they expires.
	expirationGracePeriod time.Duration
}

// NewServiceAccountReconciler creates a new ServiceAccount reconciler that creates and refreshes image pull secrets
// based on ID federation config annotated to a ServiceAccount.
// Image pull secrets are attached to a ServiceAccount (i.e. registered with .imagePullSecrets field) so that pods using
// the ServiceAccount can pull container images using the secret without specifying .spec.imagePullSecrets field.
func NewServiceAccountReconciler(
	ctx context.Context, client client.Client, scheme *runtime.Scheme, eventRecorder record.EventRecorder,
) (*serviceAccountReconciler, error) {
	g, err := newGoogle(ctx)
	if err != nil {
		return nil, err
	}

	return &serviceAccountReconciler{
		Client:                client,
		Scheme:                scheme,
		eventRecorder:         eventRecorder,
		aws:                   newAWS(),
		google:                g,
		expirationGracePeriod: time.Minute,
	}, nil
}

//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

const (
	// Event reasons.
	reasonFailedProvisioning    = "FailedProvisioningImagePullSecret"
	reasonSucceededProvisioning = "ProvisionedImagePullSecret"

	reasonFailedDecommissioning    = "FailedDecommissioningImagePullSecret"
	reasonSucceededDecommissioning = "DecommissionedImagePullSecret"
)

func (r *serviceAccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the requested ServiceAccount.
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, req.NamespacedName, sa); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Requested ServiceAccount is not found.")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "failed to get a ServiceAccount")
		return ctrl.Result{}, err
	}

	if !sa.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	var requeueAt time.Time

	if hasConfig(sa) {
		accounts := r.resolveAccounts(sa)
		for i, account := range accounts {
			exp, err := r.provisionSecretForAccount(ctx, sa, account, i, len(accounts))
			if err != nil {
				return ctrl.Result{}, err
			}
			if !exp.IsZero() && (requeueAt.IsZero() || exp.Before(requeueAt)) {
				requeueAt = exp
			}
		}
	}

	decommissioned, err := r.cleanupImagePullSecrets(ctx, logger, sa)
	if err != nil {
		r.eventRecorder.Eventf(
			sa, corev1.EventTypeWarning, reasonFailedDecommissioning,
			"Failed to decommissioning outdated image pull secrets: %v", err,
		)
		logger.Error(err, "failed to cleanup outdated image pull secrets")
		return ctrl.Result{}, err
	}
	if len(decommissioned) > 0 {
		r.eventRecorder.Eventf(
			sa, corev1.EventTypeNormal, reasonSucceededDecommissioning,
			"Decommissioned outdated image pull secrets: %v", decommissioned,
		)
	}
	if !requeueAt.IsZero() {
		return ctrl.Result{
			RequeueAfter: time.Until(requeueAt.Add(-r.expirationGracePeriod)),
		}, nil
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *serviceAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ServiceAccount{}).
		Complete(r)
}

func (r *serviceAccountReconciler) provisionSecretForAccount(
	ctx context.Context, sa *corev1.ServiceAccount, account string, accountIndex int, totalAccounts int,
) (expiresAt time.Time, _ error) {
	name := secretNameIndexed(sa, accountIndex)
	logger := log.FromContext(ctx).WithValues("secret", name)
	if totalAccounts > 1 {
		logger = logger.WithValues("accountIndex", accountIndex)
	}

	should, exp, err := r.shouldCreateOrRefreshImagePullSecret(ctx, logger, sa, name)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to determine if an image pull secret should be created or refreshed: %w", err)
	}

	if !should {
		return exp, nil
	}

	secret, newExp, err := r.createOrRefreshImagePullSecret(ctx, logger, sa, name, account)
	if err != nil {
		r.eventRecorder.Eventf(sa, corev1.EventTypeWarning, reasonFailedProvisioning, "Failed to create or refresh an image pull secret: %v", err)
		return time.Time{}, fmt.Errorf("failed to create or refresh an image pull secret: %w", err)
	}

	if err := r.attachImagePullSecret(ctx, logger, sa, secret); err != nil {
		r.eventRecorder.Eventf(sa, corev1.EventTypeWarning, reasonFailedProvisioning, "Failed to add an image pull secret to the ServiceAccount: %v", err)
		return time.Time{}, fmt.Errorf("failed to attach an image pull secret to a ServiceAccount: %w", err)
	}

	r.eventRecorder.Eventf(sa, corev1.EventTypeNormal, reasonSucceededProvisioning, "Provisioned an image pull secret: %s", secret.GetName())
	
	if !newExp.IsZero() {
		return newExp, nil
	}
	return exp, nil
}

func (r *serviceAccountReconciler) shouldCreateOrRefreshImagePullSecret(
	ctx context.Context, logger logr.Logger, sa *corev1.ServiceAccount, name string,
) (should bool, expiresAt time.Time, _ error) {
	if !hasConfig(sa) {
		logger.Info("ServiceAccount does not have configuration for image pull secret provisioning.")
		return false, time.Time{}, nil
	}

	// Check if the image pull secret exists.
	secretKey := client.ObjectKey{Namespace: sa.GetNamespace(), Name: name}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Image pull secret does not exist. Should be created.")
			return true, time.Time{}, nil
		}

		return false, time.Time{}, fmt.Errorf("failed to check the existing of an image pull secret: %w", err)
	}

	// Check if the image pull secret is attached to the ServiceAccount.
	if !r.imagePullSecretAttached(sa, secret.GetName()) {
		logger.Info("Image pull secret is not attached to the ServiceAccount. Should be attached.")
		return true, time.Time{}, nil
	}

	// Check the expiration time of the image pull secret.
	expiresAt, err := func() (time.Time, error) {
		str, ok := secret.Annotations[annotationKeyExpiresAt]
		if !ok {
			return time.Time{}, fmt.Errorf("%q annotation is missing", annotationKeyExpiresAt)
		}

		expiresAt, err := time.Parse(time.RFC3339, str)
		if err != nil {
			return time.Time{}, fmt.Errorf("failed to parse %q annotation: %w", annotationKeyExpiresAt, err)
		}

		return expiresAt, nil
	}()
	if err != nil {
		logger.Error(err, "Failed to determine the expiration of the image pull secret. Should be refreshed.")
		// Not returning an error here to continue the reconciliation and set expires-at annotation to the Secret.
		return true, time.Time{}, nil
	}

	if time.Until(expiresAt) < r.expirationGracePeriod {
		logger.Info("Image pull secret is about to expire. Should be refreshed.", "expiresAt", expiresAt)
		return true, expiresAt, nil
	}

	logger.Info("Image pull secret has enough remaining validity. Skipping refreshing it.", "expiresAt", expiresAt)
	return false, expiresAt, nil
}

func (r *serviceAccountReconciler) createOrRefreshImagePullSecret(
	ctx context.Context, logger logr.Logger, sa *corev1.ServiceAccount, name string, account string,
) (_ *corev1.Secret, expiresAt time.Time, _ error) {
	logger.Info("Creating or refreshing an image pull secret for the ServiceAccount...")
	username, token, expiresAt, err := r.generateAccessToken(ctx, sa, sa.Annotations[annotationKeyAudience], account)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to generate an access token for the configured image registry: %w", err)
	}
	logger.Info("Generated an access token for the configured image registry.", "expiresAt", expiresAt)
	secret, err := buildImagePullSecret(sa, name, sa.Annotations[annotationKeyRegistry], username, token, expiresAt)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to build image pull secret definition: %w", err)
	}
	op, err := r.ensureSecret(ctx, secret)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to ensure an image pull secret: %w", err)
	}
	logger.Info("Ensured an image pull secret.", "secret", secret.GetName(), "operation", op)
	return secret, expiresAt, nil
}

func (r *serviceAccountReconciler) attachImagePullSecret(
	ctx context.Context, logger logr.Logger, sa *corev1.ServiceAccount, secret *corev1.Secret,
) error {
	logger.Info("Attaching the image pull secret to the ServiceAccount...")

	if r.imagePullSecretAttached(sa, secret.GetName()) {
		logger.Info("Image pull secret is already attached to the ServiceAccount.")
		return nil
	}

	orig := sa.DeepCopy()
	sa.ImagePullSecrets = append(sa.ImagePullSecrets, corev1.LocalObjectReference{Name: secret.GetName()})
	if err := r.Patch(ctx, sa, client.StrategicMergeFrom(orig), client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("failed to patch a ServiceAccount: %w", err)
	}
	logger.Info("Attached the image pull secret to the ServiceAccount.")

	return nil
}

func (r *serviceAccountReconciler) cleanupImagePullSecrets(
	ctx context.Context, logger logr.Logger, sa *corev1.ServiceAccount,
) (decommissioned []string, _ error) {
	logger.Info("Cleaning up outdated image pull secrets...")

	// List image pull secrets to cleanup.
	targets, err := r.listImagePullSecretsToCleanup(ctx, sa)
	if err != nil {
		return nil, fmt.Errorf("failed to list image pull secrets to cleanup: %w", err)
	}

	if len(targets) == 0 {
		logger.Info("No image pull secrets to cleanup.")
		return nil, nil
	}

	names := []string{}
	for _, target := range targets {
		names = append(names, target.GetName())
	}
	logger.Info("Listed image pull secrets to cleanup.", "targets", names)

	// Detach the image pull secrets from the ServiceAccount.
	if err := r.detachImagePullSecret(ctx, sa, targets); err != nil {
		return nil, fmt.Errorf("failed to detach image pull secrets from a ServiceAccount: %w", err)
	}
	logger.Info("Detached image pull secrets of cleanup targets from the ServiceAccount.")

	// Delete the image pull secrets.
	for _, target := range targets {
		if err := r.Delete(ctx, target); err != nil {
			return nil, fmt.Errorf("failed to delete an image pull secret: %w", err)
		}
	}
	logger.Info("Deleted image pull secrets of cleanup targets.")

	return names, nil
}

func (r *serviceAccountReconciler) imagePullSecretAttached(sa *corev1.ServiceAccount, secretName string) bool {
	for _, ref := range sa.ImagePullSecrets {
		if ref.Name == secretName {
			return true
		}
	}

	return false
}

func (r *serviceAccountReconciler) generateAccessToken(
	ctx context.Context, sa *corev1.ServiceAccount, audience string, account string,
) (username string, token string, expiresAt time.Time, _ error) {
	tokenReq := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences: []string{audience},
		},
	}
	if err := r.SubResource("token").Create(ctx, sa, tokenReq); err != nil {
		return "", "", time.Time{}, fmt.Errorf("failed to create a ServiceAccount token: %w", err)
	}

	// AWS
	if sa.Annotations[annotationKeyAWSRoleARN] != "" {
		registry := sa.Annotations[annotationKeyRegistry]
		return r.generateAccessTokenAWS(ctx, tokenReq.Status.Token, registry, account)
	}

	// Google
	if provider := sa.Annotations[annotationKeyGoogleWIDP]; provider != "" {
		token, expiresAt, err := r.google.GenerateAccessToken(ctx, tokenReq.Status.Token, provider, account)
		if err != nil {
			return "", "", time.Time{}, fmt.Errorf("failed to generate a Google service account's access token: %w", err)
		}
		return "oauth2accesstoken", token, expiresAt, nil
	}

	return "", "", time.Time{}, errors.New("ServiceAccount is missing configuration for image pull secret provisioning")
}

func (r *serviceAccountReconciler) generateAccessTokenAWS(
	ctx context.Context, k8sToken string, registry string, roleARN string,
) (username string, token string, expiresAt time.Time, _ error) {
	region, err := r.aws.ExtractRegion(registry)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("failed to extract an AWS region from registry: %w", err)
	}

	username, password, expiresAt, err := r.aws.GenerateAccessToken(ctx, k8sToken, region, roleARN)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("failed to generate an ECR authorization token: %w", err)
	}

	return username, password, expiresAt, nil
}

func (r *serviceAccountReconciler) ensureSecret(
	ctx context.Context, desired *corev1.Secret,
) (controllerutil.OperationResult, error) {
	// We don't use controllerutil.CreateOrPatch because it does not accept client.{Create,Patch}Option.

	orig := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), orig); err != nil {
		if !apierrors.IsNotFound(err) {
			return controllerutil.OperationResultNone,
				fmt.Errorf("failed to check the existing of an image pull secret: %w", err)
		}

		if err := r.Create(ctx, desired, client.FieldOwner(fieldManager)); err != nil {
			return controllerutil.OperationResultNone,
				fmt.Errorf("failed to create an image pull secret: %w", err)
		}

		return controllerutil.OperationResultCreated, nil
	}

	if !reflect.DeepEqual(orig, desired) {
		if err := r.Patch(ctx, desired, client.StrategicMergeFrom(orig), client.FieldOwner(fieldManager)); err != nil {
			return controllerutil.OperationResultNone,
				fmt.Errorf("failed to patch an image pull secret: %w", err)
		}

		return controllerutil.OperationResultUpdated, nil
	}

	return controllerutil.OperationResultNone, nil
}

func (r *serviceAccountReconciler) listImagePullSecretsToCleanup(
	ctx context.Context, sa *corev1.ServiceAccount,
) ([]*corev1.Secret, error) {
	secrets := &corev1.SecretList{}
	if err := r.List(
		ctx,
		secrets,
		client.InNamespace(sa.GetNamespace()),
		client.MatchingLabels{
			labelKeyServiceAccount: sa.GetName(),
		},
	); err != nil {
		return nil, fmt.Errorf("failed to list image pull secrets: %w", err)
	}

	namesInUse := map[string]struct{}{}
	if hasConfig(sa) {
		accounts := r.resolveAccounts(sa)
		for i := range accounts {
			namesInUse[secretNameIndexed(sa, i)] = struct{}{}
		}
	}
	targets := []*corev1.Secret{}
	for i := range secrets.Items {
		sec := &secrets.Items[i]
		if _, ok := namesInUse[sec.GetName()]; ok {
			continue
		}
		targets = append(targets, sec)
	}
	return targets, nil
}

func (r *serviceAccountReconciler) detachImagePullSecret(
	ctx context.Context, sa *corev1.ServiceAccount, targets []*corev1.Secret,
) error {
	isTarget := func(name string) bool {
		for _, target := range targets {
			if target.GetName() == name {
				return true
			}
		}

		return false
	}

	retained := []corev1.LocalObjectReference{}
	for _, ref := range sa.ImagePullSecrets {
		if isTarget(ref.Name) {
			continue
		}

		retained = append(retained, ref)
	}

	orig := sa.DeepCopy()
	sa.ImagePullSecrets = retained
	if err := r.Patch(ctx, sa, client.StrategicMergeFrom(orig), client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("failed to patch a ServiceAccount: %w", err)
	}

	return nil
}

func (r *serviceAccountReconciler) resolveAccounts(sa *corev1.ServiceAccount) []string {
	for _, key := range []string{annotationKeyGoogleSA, annotationKeyAWSRoleARN} {
		if raw := sa.Annotations[key]; raw != "" {
			return strings.Split(raw, ",")
		}
	}
	return nil
}
