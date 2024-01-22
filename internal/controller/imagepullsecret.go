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
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// buildImagePullSecret builds a Kubernetes Secret definition for an image pull secrets.
// The built Secret will have
// - a label to select them by the ServiceAccount name,
// - an annotation to store the expiration time, and
// - an owner reference to the ServiceAccount so that they will be deleted when the ServiceAccount no longer exists.
func buildImagePullSecret(
	serviceAccount *corev1.ServiceAccount,
	secretName string,
	registry string,
	username string,
	password string,
	expiresAt time.Time,
) (*corev1.Secret, error) {
	type dockerConfigEntry struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}

	type dockerConfigJSON struct {
		Auths map[string]dockerConfigEntry `json:"auths"`
	}

	dockerCfg := &dockerConfigJSON{
		Auths: map[string]dockerConfigEntry{
			registry: {
				Username: username,
				Password: password,
			},
		},
	}

	data, err := json.Marshal(dockerCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal a Docker config JSON: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: serviceAccount.GetNamespace(),
			Name:      secretName,
			Labels: map[string]string{
				labelKeyServiceAccount: serviceAccount.GetName(),
			},
			Annotations: map[string]string{
				annotationKeyExpiresAt: expiresAt.Format(time.RFC3339),
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "ServiceAccount",
					Name:       serviceAccount.GetName(),
					UID:        serviceAccount.GetUID(),
					Controller: ptr.To(true),
				},
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		StringData: map[string]string{
			corev1.DockerConfigJsonKey: string(data),
		},
	}

	return secret, nil
}
