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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
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

func secretName(sa *corev1.ServiceAccount) string {
	if name, ok := sa.Annotations[annotationKeySecretName]; ok {
		return name
	}

	name := "imagepullsecret-" + sa.GetName()
	if len(name) > validation.DNS1123SubdomainMaxLength {
		name = name[:validation.DNS1123SubdomainMaxLength]
	}

	return name
}

func secretNameIndexed(sa *corev1.ServiceAccount, idx int) string {
	if idx <= 0 {
		return secretName(sa)
	}
	base := secretName(sa)
	suffix := fmt.Sprintf("-%d", idx)
	if len(base)+len(suffix) > validation.DNS1123SubdomainMaxLength {
		base = base[:validation.DNS1123SubdomainMaxLength-len(suffix)]
	}
	return base + suffix
}
