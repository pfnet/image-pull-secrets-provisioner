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
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type aws interface {
	// GenerateAccessToken generates an ECR authorization token from a Kubernetes ServiceAccount token.
	GenerateAccessToken(
		ctx context.Context,
		k8sServiceAccountToken string,
		region string,
		awsRoleARN string,
	) (username string, password string, expiresAt time.Time, _ error)

	// ExtractRegion extracts an AWS region from an ECR registry.
	ExtractRegion(registry string) (string, error)
}

func newAWS() aws {
	return &awsImpl{
		ecrClient: ecr.New(ecr.Options{}),
	}
}

type awsImpl struct {
	ecrClient *ecr.Client
}

func (a *awsImpl) GenerateAccessToken(
	ctx context.Context,
	k8sServiceAccountToken string,
	region string,
	awsRoleARN string,
) (username string, password string, expiresAt time.Time, _ error) {
	// With stscreds.NewWebIdentityRoleProvider, there seems to be no way to specify a region for the STS client
	// dynamically, so here we need to create a new STS client with the region specified.
	stsClient := sts.New(sts.Options{
		Region: region,
	})
	credsProvider := stscreds.NewWebIdentityRoleProvider(
		stsClient, awsRoleARN, &awsStaticIDTokenRetriever{token: k8sServiceAccountToken},
	)

	// Create an ECR authorization token.
	resp, err := a.ecrClient.GetAuthorizationToken(
		ctx, &ecr.GetAuthorizationTokenInput{},
		func(o *ecr.Options) {
			o.Region = region
			o.Credentials = credsProvider
		},
	)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("failed to get an ECR authorization token: %w", err)
	}

	if auth := resp.AuthorizationData; len(auth) != 1 {
		return "", "", time.Time{}, fmt.Errorf(
			"unexpected response from ECR GetAuthorizationToken API: length %d != 1", len(auth),
		)
	} else if auth[0].AuthorizationToken == nil {
		return "", "", time.Time{}, errors.New(
			"unexpected response from ECR GetAuthorizationToken API: AuthorizationToken is nil",
		)
	} else if auth[0].ExpiresAt == nil {
		return "", "", time.Time{}, errors.New(
			"unexpected response from ECR GetAuthorizationToken API: ExpiresAt is nil",
		)
	}

	username, password, err = a.parseECRToken(*resp.AuthorizationData[0].AuthorizationToken)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("failed to parse an ECR authorization token: %w", err)
	}

	return username, password, *resp.AuthorizationData[0].ExpiresAt, nil
}

func (a *awsImpl) ExtractRegion(registry string) (string, error) {
	// Registry <account>.dkr.ecr.<region>.amazonaws.com format.
	parts := strings.SplitN(registry, ".", 5)
	if len(parts) != 5 {
		return "", fmt.Errorf("unexpected registry format: %s", registry)
	}

	return parts[3], nil
}

// awsStaticIDTokenRetriever implements stscreds.IdentityTokenRetriever interface.
type awsStaticIDTokenRetriever struct {
	token string
}

func (a *awsStaticIDTokenRetriever) GetIdentityToken() ([]byte, error) {
	return []byte(a.token), nil
}

func (a *awsImpl) parseECRToken(token string) (username string, password string, _ error) {
	// ECR tokens are base64-encoded strings in <username>:<password> format.
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", "", err
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", errors.New("unexpected ECR authorization token format")
	}

	return parts[0], parts[1], nil
}
