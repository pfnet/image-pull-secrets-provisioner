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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	//+kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment

	ctx    context.Context
	cancel context.CancelFunc
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.Background())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		// CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
		// ErrorIfCRDPathMissing: true,

		// The BinaryAssetsDirectory is only required if you want to run the tests directly
		// without call the makefile target test. If not informed it will look for the
		// default path defined in controller-runtime which is /usr/local/kubebuilder/.
		// Note that you must have the required binaries setup under the bin directory to perform
		// the tests directly. When we run make test it will be setup and used automatically.
		BinaryAssetsDirectory: filepath.Join("..", "..", "bin", "k8s",
			fmt.Sprintf("1.28.3-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}

	var err error
	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	//+kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}: {
					Field: fields.OneTermEqualSelector("status.phase", string(corev1.PodPending)),
					Transform: func(obj any) (any, error) {
						if accessor, err := meta.Accessor(obj); err == nil {
							if accessor.GetManagedFields() != nil {
								accessor.SetManagedFields(nil)
							}
						}
						return obj, nil
					},
				},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred())

	err = (&serviceAccountReconciler{
		Client:                k8sManager.GetClient(),
		Scheme:                k8sManager.GetScheme(),
		eventRecorder:         k8sManager.GetEventRecorderFor("image-pull-secrets-provisioner"),
		aws:                   &awsMock{},
		google:                &gMock{},
		expirationGracePeriod: 0, // To test skipping refreshing Secrets.
	}).SetupWithManager(k8sManager)
	Expect(err).NotTo(HaveOccurred())

	err = (&evictor{
		Client:        k8sManager.GetClient(),
		Scheme:        k8sManager.GetScheme(),
		eventRecorder: k8sManager.GetEventRecorderFor("image-pull-secrets-provisioner"),
		requeueAfter:  100 * time.Millisecond,
	}).SetupWithManager(k8sManager)
	Expect(err).NotTo(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		err = k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred(), "failed to run manager")
	}()
})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

const tokenValidity = 5 * time.Second

// awsMock is a mock implementation of aws.
// Generated access tokens have validity of tokenValidity.
type awsMock struct {
}

func (a *awsMock) GenerateAccessToken(
	_ context.Context,
	k8sServiceAccountToken string,
	region string,
	awsRoleARN string,
) (username string, password string, expiresAt time.Time, _ error) {
	token, err := randomString()
	if err != nil {
		return "", "", time.Time{}, err
	}

	return "AWS", token, time.Now().Add(tokenValidity), nil
}

func (a *awsMock) ExtractRegion(registry string) (string, error) {
	parts := strings.SplitN(registry, ".", 5)
	if len(parts) != 5 {
		return "", fmt.Errorf("unexpected registry format: %s", registry)
	}

	return parts[3], nil
}

// gMock is a mock implementation of google.
// Generated access tokens have validity of tokenValidity.
type gMock struct {
}

func (g *gMock) GenerateAccessToken(
	_ context.Context,
	k8sServiceAccountToken string,
	workloadIdentityProvider string,
	googleServiceAccountEmail string,
) (token string, expiresAt time.Time, _ error) {
	token, err := randomString()
	if err != nil {
		return "", time.Time{}, err
	}

	return token, time.Now().Add(tokenValidity), nil
}

func randomString() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}

	return hex.EncodeToString(bytes), nil
}
