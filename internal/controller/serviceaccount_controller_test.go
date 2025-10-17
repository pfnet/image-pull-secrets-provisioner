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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("ServiceAccountReconciler", func() {
	ctx := context.Background()

	const ns = "testing"
	objectsToDelete := []client.Object{}

	extractNames := func(refs []corev1.LocalObjectReference) []string {
		names := make([]string, len(refs))
		for i, ref := range refs {
			names[i] = ref.Name
		}
		return names
	}

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
			Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
		}
		objectsToDelete = nil
	})

	Context("Google", func() {
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
			ImagePullSecrets: []corev1.LocalObjectReference{
				{
					Name: "static",
				},
			},
		}

		It("Create and attach multiple Secrets (two emails)", func() {
			// Create a ServiceAccount with two emails.
			sa2 := sa.DeepCopy()
			sa2.Annotations["imagepullsecrets.preferred.jp/googlecloud-service-account-email"] = "gsa1@example.iam.gserviceaccount.com,gsa2@example.iam.gserviceaccount.com"
			Expect(k8sClient.Create(ctx, sa2)).NotTo(HaveOccurred())
			objectsToDelete = append(objectsToDelete, sa2)

			var secretsNames []string
			Eventually(func(g Gomega) {
				secrets := &corev1.SecretList{}
				g.Expect(k8sClient.List(
					ctx,
					secrets,
					client.InNamespace(ns),
					client.MatchingLabels{
						"imagepullsecrets.preferred.jp/service-account": sa2.GetName(),
					},
				)).NotTo(HaveOccurred())
				g.Expect(secrets.Items).To(HaveLen(2))
				secretsNames = []string{secrets.Items[0].GetName(), secrets.Items[1].GetName()}
			}).Should(Succeed())
			// Base name must not have suffix, second must have -1 suffix.
			Expect(secretsNames[0]).NotTo(ContainSubstring("-1"))
			Expect(secretsNames[1]).To(ContainSubstring("-1"))

			Eventually(func(g Gomega) {
				actual := &corev1.ServiceAccount{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sa2), actual)).NotTo(HaveOccurred())
				refs := extractNames(actual.ImagePullSecrets)
				g.Expect(refs).To(ContainElements("static", secretsNames[0], secretsNames[1]))
			}).Should(Succeed())
		})

		It("Cleanup a removed email (two -> one)", func() {
			// Initial SA with two emails.
			sa2 := sa.DeepCopy()
			sa2.Annotations["imagepullsecrets.preferred.jp/googlecloud-service-account-email"] = "gsa1@example.iam.gserviceaccount.com,gsa2@example.iam.gserviceaccount.com"
			Expect(k8sClient.Create(ctx, sa2)).NotTo(HaveOccurred())
			objectsToDelete = append(objectsToDelete, sa2)

			// Wait for two secrets.
			var baseSecretName string
			var secondSecretName string
			Eventually(func(g Gomega) {
				secrets := &corev1.SecretList{}
				g.Expect(k8sClient.List(
					ctx,
					secrets,
					client.InNamespace(ns),
					client.MatchingLabels{
						"imagepullsecrets.preferred.jp/service-account": sa2.GetName(),
					},
				)).NotTo(HaveOccurred())
				g.Expect(secrets.Items).To(HaveLen(2))
				for _, s := range secrets.Items {
					if strings.Contains(s.GetName(), "-1") {
						secondSecretName = s.GetName()
					} else {
						baseSecretName = s.GetName()
					}
				}
			}).Should(Succeed())

			// Remove second email.
			orig := sa2.DeepCopy()
			sa2.Annotations["imagepullsecrets.preferred.jp/googlecloud-service-account-email"] = "gsa1@example.iam.gserviceaccount.com"
			Expect(k8sClient.Patch(ctx, sa2, client.StrategicMergeFrom(orig))).NotTo(HaveOccurred())

			// Expect only base secret remains and second is deleted + detached.
			Eventually(func(g Gomega) {
				secrets := &corev1.SecretList{}
				g.Expect(k8sClient.List(
					ctx,
					secrets,
					client.InNamespace(ns),
					client.MatchingLabels{
						"imagepullsecrets.preferred.jp/service-account": sa2.GetName(),
					},
				)).NotTo(HaveOccurred())
				g.Expect(secrets.Items).To(HaveLen(1))
				g.Expect(secrets.Items[0].GetName()).To(Equal(baseSecretName))
			}).Should(Succeed())

			Eventually(func(g Gomega) {
				actual := &corev1.ServiceAccount{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sa2), actual)).NotTo(HaveOccurred())
				refs := extractNames(actual.ImagePullSecrets)
				g.Expect(refs).To(ContainElements("static", baseSecretName))
				g.Expect(refs).NotTo(ContainElement(secondSecretName))
			}).Should(Succeed())
		})

		It("Refresh multiple Secrets (two emails)", func() {
			// Create a ServiceAccount with two emails.
			sa2 := sa.DeepCopy()
			sa2.Annotations["imagepullsecrets.preferred.jp/googlecloud-service-account-email"] = "gsa1@example.iam.gserviceaccount.com,gsa2@example.iam.gserviceaccount.com"
			Expect(k8sClient.Create(ctx, sa2)).NotTo(HaveOccurred())
			objectsToDelete = append(objectsToDelete, sa2)

			// Wait for two secrets are created.
			var secrets []*corev1.Secret
			Eventually(func(g Gomega) {
				secretList := &corev1.SecretList{}
				g.Expect(k8sClient.List(
					ctx,
					secretList,
					client.InNamespace(ns),
					client.MatchingLabels{
						"imagepullsecrets.preferred.jp/service-account": sa2.GetName(),
					},
				)).NotTo(HaveOccurred())
				g.Expect(secretList.Items).To(HaveLen(2))
				secrets = []*corev1.Secret{&secretList.Items[0], &secretList.Items[1]}
			}).Should(Succeed())

			// Test that both Secrets are refreshed.
			Eventually(func(g Gomega) {
				for _, secret := range secrets {
					actual := &corev1.Secret{}
					g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), actual)).NotTo(HaveOccurred())
					g.Expect(actual.Data).NotTo(Equal(secret.Data))
				}
			}).WithTimeout(2 * tokenValidity).Should(Succeed())
		})

		It("Cleanup outdated Secrets (multiple principals)", func() {
			// Create a ServiceAccount with two emails.
			sa2 := sa.DeepCopy()
			sa2.Annotations["imagepullsecrets.preferred.jp/googlecloud-service-account-email"] = "gsa1@example.iam.gserviceaccount.com,gsa2@example.iam.gserviceaccount.com"
			Expect(k8sClient.Create(ctx, sa2)).NotTo(HaveOccurred())
			objectsToDelete = append(objectsToDelete, sa2)

			// Wait for two secrets are created.
			Eventually(func(g Gomega) {
				secrets := &corev1.SecretList{}
				g.Expect(k8sClient.List(
					ctx,
					secrets,
					client.InNamespace(ns),
					client.MatchingLabels{
						"imagepullsecrets.preferred.jp/service-account": sa2.GetName(),
					},
				)).NotTo(HaveOccurred())
				g.Expect(secrets.Items).To(HaveLen(2))
			}).Should(Succeed())

			// Create an outdated secret.
			outdated := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sa2.GetName() + "-outdated",
					Namespace: ns,
					Labels: map[string]string{
						"imagepullsecrets.preferred.jp/service-account": sa2.GetName(),
					},
				},
				Data: map[string][]byte{".dockerconfigjson": []byte("{}")},
				Type: corev1.SecretTypeDockerConfigJson,
			}
			Expect(k8sClient.Create(ctx, outdated)).NotTo(HaveOccurred())
			// Note: outdated secret is not added to objectsToDelete because it will be deleted by reconciliation

			// Trigger reconciliation.
			orig := sa2.DeepCopy()
			sa2.Annotations["trigger-reconcile"] = time.Now().Format(time.RFC3339)
			Expect(k8sClient.Patch(ctx, sa2, client.StrategicMergeFrom(orig))).NotTo(HaveOccurred())

			// Expect outdated secret is cleaned up.
			Eventually(func(g Gomega) {
				secrets := &corev1.SecretList{}
				g.Expect(k8sClient.List(
					ctx,
					secrets,
					client.InNamespace(ns),
					client.MatchingLabels{
						"imagepullsecrets.preferred.jp/service-account": sa2.GetName(),
					},
				)).NotTo(HaveOccurred())
				g.Expect(secrets.Items).To(HaveLen(2))
				for _, s := range secrets.Items {
					g.Expect(s.GetName()).NotTo(Equal(outdated.GetName()))
				}
			}).Should(Succeed())
		})

		It("Create and attach a Secret", func() {
			// Create a ServiceAccount.
			sa := sa.DeepCopy()
			Expect(k8sClient.Create(ctx, sa)).NotTo(HaveOccurred())
			objectsToDelete = append(objectsToDelete, sa)

			// Test that a Secret is created.
			secret := &corev1.Secret{}
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

				secret = &secrets.Items[0]
			}).Should(Succeed())
			Expect(secret.Annotations).To(HaveKey("imagepullsecrets.preferred.jp/expires-at"))

			// Test that the Secret is attached to the ServiceAccount.
			Eventually(func(g Gomega) {
				actual := &corev1.ServiceAccount{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sa), actual)).NotTo(HaveOccurred())

				g.Expect(actual.ImagePullSecrets).To(WithTransform(extractNames, ConsistOf("static", secret.GetName())))
			}).Should(Succeed())
		})

		It("Skip refreshing Secrets", func() {
			// Create a ServiceAccount.
			sa := sa.DeepCopy()
			Expect(k8sClient.Create(ctx, sa)).NotTo(HaveOccurred())
			objectsToDelete = append(objectsToDelete, sa)

			// Test that a Secret is created.
			secret := &corev1.Secret{}
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

				secret = &secrets.Items[0]
			}).Should(Succeed())
			Expect(secret.Annotations).To(HaveKey("imagepullsecrets.preferred.jp/expires-at"))

			// Test that the Secret is not refreshed while it is valid.
			Consistently(func(g Gomega) {
				orig := sa.DeepCopy()
				sa.Annotations["trigger-reconcile"] = time.Now().Format(time.RFC3339)
				Expect(k8sClient.Patch(ctx, sa, client.StrategicMergeFrom(orig))).NotTo(HaveOccurred())

				actual := &corev1.Secret{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), actual)).NotTo(HaveOccurred())

				g.Expect(actual.Data).To(Equal(secret.Data))
			}, tokenValidity*800/1000).Should(Succeed()) // 0.8 x token validity.
		})

		It("Refresh a Secret", func() {
			// Create a ServiceAccount.
			sa := sa.DeepCopy()
			Expect(k8sClient.Create(ctx, sa)).NotTo(HaveOccurred())
			objectsToDelete = append(objectsToDelete, sa)

			// Wait for a Secret is created once.
			secret := &corev1.Secret{}
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

				secret = &secrets.Items[0]
			}).Should(Succeed())

			// Test that the Secret is refreshed.
			Eventually(func(g Gomega) {
				actual := &corev1.Secret{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(secret), actual)).NotTo(HaveOccurred())
				g.Expect(actual.Annotations).To(HaveKey("imagepullsecrets.preferred.jp/expires-at"))

				g.Expect(actual.Data).NotTo(Equal(secret.Data))
			}).WithTimeout(2 * tokenValidity).Should(Succeed()) // 2 x token validity.
		})

		It("Cleanup outdated Secrets", func() {
			// Create a ServiceAccount.
			sa := sa.DeepCopy()
			Expect(k8sClient.Create(ctx, sa)).NotTo(HaveOccurred())
			objectsToDelete = append(objectsToDelete, sa)

			// Wait for a Secret is created once.
			outdated := &corev1.Secret{}
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

				outdated = &secrets.Items[0]
			}).Should(Succeed())

			// Change the name of Secret to provision.
			orig := sa.DeepCopy()
			sa.Annotations["imagepullsecrets.preferred.jp/secret-name"] = "imagepullsecret-2"
			Expect(k8sClient.Patch(ctx, sa, client.StrategicMergeFrom(orig))).NotTo(HaveOccurred())

			// Test that a new Secret is created and the outdated Secret is deleted.
			secret := &corev1.Secret{}
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

				secret = &secrets.Items[0]
				g.Expect(secret.GetName()).NotTo(Equal(outdated.GetName()))
			}).Should(Succeed())
			Expect(secret.Annotations).To(HaveKey("imagepullsecrets.preferred.jp/expires-at"))

			// Test that the new Secret is attached to the ServiceAccount and the outdated Secret is detached.
			Eventually(func(g Gomega) {
				actual := &corev1.ServiceAccount{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sa), actual)).NotTo(HaveOccurred())

				g.Expect(actual.ImagePullSecrets).To(WithTransform(extractNames, ConsistOf("static", secret.GetName())))
			}).Should(Succeed())
		})

		It("Cleanup all Secrets", func() {
			// Create a ServiceAccount.
			sa := sa.DeepCopy()
			Expect(k8sClient.Create(ctx, sa)).NotTo(HaveOccurred())
			objectsToDelete = append(objectsToDelete, sa)

			// Wait for a Secret is created once.
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

			// Remove the config for image pull secret provisioning.
			orig := sa.DeepCopy()
			sa.Annotations["imagepullsecrets.preferred.jp/googlecloud-service-account-email"] = ""
			Expect(k8sClient.Patch(ctx, sa, client.StrategicMergeFrom(orig))).NotTo(HaveOccurred())

			// Test that the Secret is deleted.
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
				g.Expect(secrets.Items).To(BeEmpty())
			}).Should(Succeed())

			// Test that the Secret is detached from the ServiceAccount.
			Eventually(func(g Gomega) {
				actual := &corev1.ServiceAccount{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sa), actual)).NotTo(HaveOccurred())

				g.Expect(actual.ImagePullSecrets).To(WithTransform(extractNames, ConsistOf("static")))
			}).Should(Succeed())
		})
	})

	Context("AWS", func() {
		sa := &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      "sa-0",
				Annotations: map[string]string{
					"imagepullsecrets.preferred.jp/registry":     "999999999999.dkr.ecr.ap-northeast-1.amazonaws.com",
					"imagepullsecrets.preferred.jp/audience":     "sts.amazonaws.com",
					"imagepullsecrets.preferred.jp/aws-role-arn": "arn:aws:iam::999999999999:role/role-name",
				},
			},
			ImagePullSecrets: []corev1.LocalObjectReference{
				{
					Name: "static",
				},
			},
		}

		It("Create and attach a Secret", func() {
			// Create a ServiceAccount.
			sa := sa.DeepCopy()
			Expect(k8sClient.Create(ctx, sa)).NotTo(HaveOccurred())
			objectsToDelete = append(objectsToDelete, sa)

			// Test that a Secret is created.
			secret := &corev1.Secret{}
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

				secret = &secrets.Items[0]
			}).Should(Succeed())
			Expect(secret.Annotations).To(HaveKey("imagepullsecrets.preferred.jp/expires-at"))

			// Test that the Secret is attached to the ServiceAccount.
			Eventually(func(g Gomega) {
				actual := &corev1.ServiceAccount{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sa), actual)).NotTo(HaveOccurred())

				g.Expect(actual.ImagePullSecrets).To(WithTransform(extractNames, ConsistOf("static", secret.GetName())))
			}).Should(Succeed())
		})

		It("AWS-specific: comma-separated Role ARNs", func() {
			// Test AWS comma-separated parsing (vs Google's behavior).
			sa2 := sa.DeepCopy()
			sa2.Name = "sa-aws-multi"
			sa2.Annotations["imagepullsecrets.preferred.jp/aws-role-arn"] = "arn:aws:iam::999999999999:role/role-1,arn:aws:iam::999999999999:role/role-2"
			Expect(k8sClient.Create(ctx, sa2)).NotTo(HaveOccurred())
			objectsToDelete = append(objectsToDelete, sa2)

			// Verify AWS creates 2 secrets with correct naming.
			Eventually(func(g Gomega) {
				secrets := &corev1.SecretList{}
				g.Expect(k8sClient.List(ctx, secrets, client.InNamespace(ns), client.MatchingLabels{
					"imagepullsecrets.preferred.jp/service-account": sa2.GetName(),
				})).NotTo(HaveOccurred())
				g.Expect(secrets.Items).To(HaveLen(2))

				// AWS naming: first no suffix, second "-1" suffix.
				names := []string{secrets.Items[0].GetName(), secrets.Items[1].GetName()}
				g.Expect(names[0]).NotTo(ContainSubstring("-1"))
				g.Expect(names[1]).To(ContainSubstring("-1"))
			}).Should(Succeed())
		})
		// Other test cases are omitted because they are covered by the Google test cases.
	})
})
