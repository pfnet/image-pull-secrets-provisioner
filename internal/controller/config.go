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
	"fmt"
	"hash/fnv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/rand"
)

// Helpers for config annotations.

func hasConfig(sa *corev1.ServiceAccount) bool {
	// Common.
	if sa.Annotations[annotationKeyRegistry] == "" {
		return false
	}
	if sa.Annotations[annotationKeyAudience] == "" {
		return false
	}

	// AWS.
	if sa.Annotations[annotationKeyAWSRoleARN] != "" {
		return true
	}

	// Google.
	if sa.Annotations[annotationKeyGoogleWIDP] != "" {
		if sa.Annotations[annotationKeyGoogleSA] != "" {
			return true
		}
	}

	return false
}

func configHash(sa *corev1.ServiceAccount) string {
	hasher := fnv.New32a()

	for _, key := range []string{
		annotationKeyRegistry,
		annotationKeyAudience,
		annotationKeyAWSRoleARN,
		annotationKeyGoogleWIDP,
		annotationKeyGoogleSA,
	} {
		hasher.Write([]byte(sa.Annotations[key]))
	}

	return rand.SafeEncodeString(fmt.Sprint(hasher.Sum32()))
}

func secretName(sa *corev1.ServiceAccount) string {
	// TODO: Consider name confliction with manual creation or other provisioning system.
	return fmt.Sprintf("imagepullsecret-%s-%s", sa.GetName(), configHash(sa))
}
