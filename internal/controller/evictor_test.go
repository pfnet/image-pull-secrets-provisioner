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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Evictor", func() {
	ctx := context.Background()

	const ns = "testing"
	objectsToDelete := []client.Object{}

	BeforeEach(func() {
		err := k8sClient.Create(
			ctx,
			&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: ns,
				},
			},
		)
		if !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterEach(func() {
		for _, obj := range objectsToDelete {
			err := k8sClient.Delete(ctx, obj)
			Expect(client.IgnoreNotFound(err)).NotTo(HaveOccurred())
		}
		objectsToDelete = nil
	})

	It("Evict a target pod", func() {
		// Create a ServiceAccount.
		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns,
				GenerateName: "sa-",
			},
		}
		Expect(k8sClient.Create(ctx, sa)).NotTo(HaveOccurred())
		objectsToDelete = append(objectsToDelete, sa)

		// Create a pod that uses the ServiceAccount.
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns,
				GenerateName: "pod-",
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: sa.GetName(),
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "busybox",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).NotTo(HaveOccurred())
		objectsToDelete = append(objectsToDelete, pod)

		// Add configuration for image pull secret provisioning to the ServiceAccount.
		// The existing pod will not have an image pull secret provisioned.
		orig := sa.DeepCopy()
		sa.Annotations = map[string]string{
			"imagepullsecrets.preferred.jp/registry":                               "asia-northeas1-docker.pkg.dev",
			"imagepullsecrets.preferred.jp/audience":                               "//iam.googleapis.com/projects/999999999999/locations/global/workloadIdentityPools/pool-name/providers/provider-name",
			"imagepullsecrets.preferred.jp/googlecloud-workload-identity-provider": "projects/999999999999/locations/global/workloadIdentityPools/pool-name/providers/provider-name",
			"imagepullsecrets.preferred.jp/googlecloud-service-account-email":      "imagepullsecret@example.iam.gserviceaccount.com",
		}
		Expect(k8sClient.Patch(ctx, sa, client.StrategicMergeFrom(orig))).NotTo(HaveOccurred())

		// Wait for an image pull secret to be created.
		Eventually(func(g Gomega) {
			secrets := &corev1.SecretList{}
			g.Expect(k8sClient.List(
				ctx,
				secrets,
				client.InNamespace(ns),
				client.MatchingLabels{
					"imagepullsecrets.preferred.jp/service-account": sa.GetName(),
				},
			)).NotTo(HaveOccurred())
			g.Expect(secrets.Items).To(HaveLen(1))
		}).Should(Succeed())

		// Test that the pod has been evicted.
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
	})

	It("Not evict a non-target pod", func() {
		// Create a ServiceAccount.
		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns,
				GenerateName: "sa-",
				Annotations: map[string]string{
					"imagepullsecrets.preferred.jp/registry":                               "asia-northeas1-docker.pkg.dev",
					"imagepullsecrets.preferred.jp/audience":                               "//iam.googleapis.com/projects/999999999999/locations/global/workloadIdentityPools/pool-name/providers/provider-name",
					"imagepullsecrets.preferred.jp/googlecloud-workload-identity-provider": "projects/999999999999/locations/global/workloadIdentityPools/pool-name/providers/provider-name",
					"imagepullsecrets.preferred.jp/googlecloud-service-account-email":      "imagepullsecret@example.iam.gserviceaccount.com",
				},
			},
		}
		Expect(k8sClient.Create(ctx, sa)).NotTo(HaveOccurred())
		objectsToDelete = append(objectsToDelete, sa)

		// Wait for an image pull secret to be created.
		secret := ""
		Eventually(func(g Gomega) {
			secrets := &corev1.SecretList{}
			g.Expect(k8sClient.List(
				ctx,
				secrets,
				client.InNamespace(ns),
				client.MatchingLabels{
					"imagepullsecrets.preferred.jp/service-account": sa.GetName(),
				},
			)).NotTo(HaveOccurred())
			g.Expect(secrets.Items).To(HaveLen(1))
			secret = secrets.Items[0].GetName()
		}).Should(Succeed())

		// Create a pod that uses the ServiceAccount.
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns,
				GenerateName: "pod-",
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: sa.GetName(),
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "busybox",
					},
				},
				// Envtest does not propagate image pull secrets, so we add it manually.
				ImagePullSecrets: []corev1.LocalObjectReference{
					{
						Name: secret,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).NotTo(HaveOccurred())
		objectsToDelete = append(objectsToDelete, pod)

		// Kick the reconciliation of the ServiceAccount.
		orig := sa.DeepCopy()
		sa.Annotations["reconcile"] = "true"
		Expect(k8sClient.Patch(ctx, sa, client.StrategicMergeFrom(orig))).NotTo(HaveOccurred())

		// Test that the pod remains.
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})).NotTo(HaveOccurred())
		}, time.Second).Should(Succeed())
	})

	It("Evict a target pod with multiple principals", func() {
		// Create a ServiceAccount.
		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns,
				GenerateName: "sa-",
			},
		}
		Expect(k8sClient.Create(ctx, sa)).NotTo(HaveOccurred())
		objectsToDelete = append(objectsToDelete, sa)

		// Create a pod that uses the ServiceAccount.
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns,
				GenerateName: "pod-",
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: sa.GetName(),
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "busybox",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).NotTo(HaveOccurred())
		objectsToDelete = append(objectsToDelete, pod)

		// Add configuration for image pull secret provisioning with multiple principals to the ServiceAccount.
		// The existing pod will not have image pull secrets provisioned.
		orig := sa.DeepCopy()
		sa.Annotations = map[string]string{
			"imagepullsecrets.preferred.jp/registry":                               "asia-northeas1-docker.pkg.dev",
			"imagepullsecrets.preferred.jp/audience":                               "//iam.googleapis.com/projects/999999999999/locations/global/workloadIdentityPools/pool-name/providers/provider-name",
			"imagepullsecrets.preferred.jp/googlecloud-workload-identity-provider": "projects/999999999999/locations/global/workloadIdentityPools/pool-name/providers/provider-name",
			"imagepullsecrets.preferred.jp/googlecloud-service-account-email":      "sa1@example.iam.gserviceaccount.com,sa2@example.iam.gserviceaccount.com",
		}
		Expect(k8sClient.Patch(ctx, sa, client.StrategicMergeFrom(orig))).NotTo(HaveOccurred())

		// Wait for image pull secrets to be created.
		Eventually(func(g Gomega) {
			secrets := &corev1.SecretList{}
			g.Expect(k8sClient.List(
				ctx,
				secrets,
				client.InNamespace(ns),
				client.MatchingLabels{
					"imagepullsecrets.preferred.jp/service-account": sa.GetName(),
				},
			)).NotTo(HaveOccurred())
			g.Expect(secrets.Items).To(HaveLen(2))
		}).Should(Succeed())

		// Test that the pod has been evicted.
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
	})

	It("Not evict a pod with all required secrets for multiple principals", func() {
		// Create a ServiceAccount.
		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns,
				GenerateName: "sa-",
				Annotations: map[string]string{
					"imagepullsecrets.preferred.jp/registry":                               "asia-northeas1-docker.pkg.dev",
					"imagepullsecrets.preferred.jp/audience":                               "//iam.googleapis.com/projects/999999999999/locations/global/workloadIdentityPools/pool-name/providers/provider-name",
					"imagepullsecrets.preferred.jp/googlecloud-workload-identity-provider": "projects/999999999999/locations/global/workloadIdentityPools/pool-name/providers/provider-name",
					"imagepullsecrets.preferred.jp/googlecloud-service-account-email":      "sa1@example.iam.gserviceaccount.com,sa2@example.iam.gserviceaccount.com",
				},
			},
		}
		Expect(k8sClient.Create(ctx, sa)).NotTo(HaveOccurred())
		objectsToDelete = append(objectsToDelete, sa)

		// Wait for image pull secrets to be created.
		var secrets []string
		Eventually(func(g Gomega) {
			secretList := &corev1.SecretList{}
			g.Expect(k8sClient.List(
				ctx,
				secretList,
				client.InNamespace(ns),
				client.MatchingLabels{
					"imagepullsecrets.preferred.jp/service-account": sa.GetName(),
				},
			)).NotTo(HaveOccurred())
			g.Expect(secretList.Items).To(HaveLen(2))
			secrets = []string{secretList.Items[0].GetName(), secretList.Items[1].GetName()}
		}).Should(Succeed())

		// Create a pod that uses the ServiceAccount with all required secrets.
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:    ns,
				GenerateName: "pod-",
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: sa.GetName(),
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "busybox",
					},
				},
				// Envtest does not propagate image pull secrets, so we add them manually.
				ImagePullSecrets: []corev1.LocalObjectReference{
					{Name: secrets[0]},
					{Name: secrets[1]},
				},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).NotTo(HaveOccurred())
		objectsToDelete = append(objectsToDelete, pod)

		// Kick the reconciliation of the ServiceAccount.
		orig := sa.DeepCopy()
		sa.Annotations["reconcile"] = "true"
		Expect(k8sClient.Patch(ctx, sa, client.StrategicMergeFrom(orig))).NotTo(HaveOccurred())

		// Test that the pod remains.
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})).NotTo(HaveOccurred())
		}, time.Second).Should(Succeed())
	})
})
