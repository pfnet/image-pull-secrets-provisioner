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
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
	"google.golang.org/api/sts/v1"
)

type google interface {
	// GenerateAccessToken generates a Google service account's short-lived access token from a Kubernetes
	// ServiceAccount token.
	GenerateAccessToken(
		ctx context.Context,
		k8sServiceAccountToken string,
		workloadIdentityProvider string,
		googleServiceAccountEmail string,
	) (token string, expiresAt time.Time, _ error)
}

type goog struct {
	stsClient *sts.Service
}

func newGoogle(ctx context.Context) (google, error) {
	stsClient, err := sts.NewService(ctx, option.WithoutAuthentication())
	if err != nil {
		return nil, fmt.Errorf("failed to create a Google STS client: %w", err)
	}

	return &goog{
		stsClient: stsClient,
	}, nil
}

func (g *goog) GenerateAccessToken(
	ctx context.Context,
	k8sServiceAccountToken string,
	workloadIdentityProvider string,
	googleServiceAccountEmail string,
) (token string, expiresAt time.Time, _ error) {
	// Exchange the ServiceAccount token for a Google OAuth 2.0 access token.
	stsResp, err := g.stsClient.V1.Token(&sts.GoogleIdentityStsV1ExchangeTokenRequest{
		Audience:           "//iam.googleapis.com/" + workloadIdentityProvider,
		GrantType:          "urn:ietf:params:oauth:grant-type:token-exchange",
		RequestedTokenType: "urn:ietf:params:oauth:token-type:access_token",
		Scope:              "https://www.googleapis.com/auth/iam",
		SubjectToken:       k8sServiceAccountToken,
		SubjectTokenType:   "urn:ietf:params:oauth:token-type:jwt",
	}).Context(ctx).Do()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to exchange a ServiceAccount token for a Google OAuth 2.0 access token: %w", err)
	}

	// Impersonate to a Google service account and generate an access token.
	opt := option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: stsResp.AccessToken,
		Expiry:      time.Now().Add(time.Duration(stsResp.ExpiresIn) * time.Second),
	}))
	iamCredClient, err := iamcredentials.NewService(ctx, opt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create a Google IAM Credentials client: %w", err)
	}

	tokenResp, err := iamCredClient.Projects.ServiceAccounts.GenerateAccessToken(
		"projects/-/serviceAccounts/"+googleServiceAccountEmail,
		&iamcredentials.GenerateAccessTokenRequest{
			Scope: []string{"https://www.googleapis.com/auth/cloud-platform.read-only"},
		},
	).Context(ctx).Do()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to generate a Google service account's access token: %w", err)
	}

	expiresAt, err = time.Parse(time.RFC3339, tokenResp.ExpireTime)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to parse a timestamp: %w", err)
	}

	return tokenResp.AccessToken, expiresAt, nil
}
