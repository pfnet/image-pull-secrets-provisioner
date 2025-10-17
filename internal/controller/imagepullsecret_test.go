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
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestBuildImagePullSecret(t *testing.T) {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "namespace-0",
			Name:      "serviceaccount-0",
			UID:       "uid-0",
		},
	}
	registry := "asia-northeast1-docker.pkg.dev"
	username := "oauth2accesstoken"
	password := "0xc0bebeef"
	principal := "sa@example.iam.gserviceaccount.com"
	expiresAt := time.Now().Add(time.Hour)

	actual, err := buildImagePullSecret(sa, "secret-0", registry, username, password, principal, expiresAt)
	if err != nil {
		t.Errorf("Failed to build an image pull secret: %v", err)
	}

	expectedObjectMeta := metav1.ObjectMeta{
		Namespace: "namespace-0",
		Name:      "secret-0",
		Labels: map[string]string{
			"imagepullsecrets.preferred.jp/service-account": "serviceaccount-0",
		},
		Annotations: map[string]string{
			"imagepullsecrets.preferred.jp/principal":  principal,
			"imagepullsecrets.preferred.jp/expires-at": expiresAt.Format(time.RFC3339),
		},
		OwnerReferences: []metav1.OwnerReference{
			{
				APIVersion: "v1",
				Kind:       "ServiceAccount",
				Name:       "serviceaccount-0",
				UID:        "uid-0",
				Controller: ptr.To(true),
			},
		},
	}
	if diff := cmp.Diff(expectedObjectMeta, actual.ObjectMeta); diff != "" {
		t.Errorf("ObjectMeta mismatch (-want +got):\n%s", diff)
	}

	if actual.Type != corev1.SecretTypeDockerConfigJson {
		t.Errorf("Type mismatch\n\texpected: %s\n\tactual: %s", corev1.SecretTypeDockerConfigJson, actual.Type)
	}

	expectedData := fmt.Sprintf(`{
	"auths": {
		"%s": {
			"username": "%s",
			"password": "%s"
		}
	}
}`, registry, username, password)

	actualData := &bytes.Buffer{}
	if err := json.Indent(actualData, []byte(actual.StringData[corev1.DockerConfigJsonKey]), "", "\t"); err != nil {
		t.Fatalf("Failed to indent a JSON: %v", err)
	}

	if diff := cmp.Diff(expectedData, actualData.String()); diff != "" {
		t.Errorf("Data mismatch (-want +got):\n%s", diff)
	}
}
