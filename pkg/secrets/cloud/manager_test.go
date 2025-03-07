// Copyright 2016-2022, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloud

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gocloud.dev/secrets"
	"gocloud.dev/secrets/driver"
)

// the main testing function, takes a kms url and tries to make a new secret manager out of it and encrypt and
// decrypt data
func testURL(ctx context.Context, t *testing.T, url string) {
	dataKey, err := generateNewDataKey(url)
	require.NoError(t, err)

	manager, err := newCloudSecretsManager(url, dataKey)
	require.NoError(t, err)

	enc, err := manager.Encrypter()
	require.NoError(t, err)

	dec, err := manager.Decrypter()
	require.NoError(t, err)

	ciphertext, err := enc.EncryptValue(ctx, "plaintext")
	require.NoError(t, err)

	plaintext, err := dec.DecryptValue(ctx, ciphertext)
	require.NoError(t, err)
	assert.Equal(t, "plaintext", plaintext)
}

func randomName(t *testing.T) string {
	name := ""
	letters := "abcdefghijklmnopqrstuvwxyz"
	for i := 0; i < 32; i++ {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		require.NoError(t, err)

		char := letters[j.Int64()]
		name = name + string(char)
	}
	return name
}

func getAwsCaller(t *testing.T) (context.Context, aws.Config, *sts.GetCallerIdentityOutput) {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		t.Logf("Skipping, could not load aws config: %s", err)
		t.SkipNow()
	}

	stsClient := sts.NewFromConfig(cfg)
	caller, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Logf("Skipping, couldn't use aws credentials to query identity: %s", err)
		t.SkipNow()
	}

	return ctx, cfg, caller
}

func createKey(ctx context.Context, t *testing.T, cfg aws.Config) *kms.CreateKeyOutput {
	kmsClient := kms.NewFromConfig(cfg)
	keyName := "test-key-" + randomName(t)
	key, err := kmsClient.CreateKey(ctx, &kms.CreateKeyInput{Description: &keyName})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := kmsClient.ScheduleKeyDeletion(ctx, &kms.ScheduleKeyDeletionInput{
			KeyId: key.KeyMetadata.KeyId,
		})
		assert.NoError(t, err)
	})

	return key
}

//nolint:paralleltest // mutates environment variables
func TestAWSCloudManager(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	ctx, cfg, _ := getAwsCaller(t)

	key := createKey(ctx, t, cfg)
	url := "awskms://" + *key.KeyMetadata.KeyId + "?awssdk=v2"

	testURL(ctx, t, url)
}

//nolint:paralleltest // mutates environment variables
func TestAWSCloudManager_SessionToken(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	ctx, cfg, _ := getAwsCaller(t)

	key := createKey(ctx, t, cfg)
	url := "awskms://" + *key.KeyMetadata.KeyId + "?awssdk=v2"

	creds, err := cfg.Credentials.Retrieve(ctx)
	require.NoError(t, err)

	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", creds.AccessKeyID)
	t.Setenv("AWS_SECRET_ACCESS_KEY", creds.SecretAccessKey)
	t.Setenv("AWS_SESSION_TOKEN", creds.SessionToken)

	testURL(ctx, t, url)
}

//nolint:paralleltest // mutates environment variables
func TestAWSCloudManager_AssumedRole(t *testing.T) {
	// Regression test for https://github.com/pulumi/pulumi/issues/11482
	t.Setenv("AWS_REGION", "us-west-2")
	ctx, cfg, caller := getAwsCaller(t)

	// Make a key with our default config
	key := createKey(ctx, t, cfg)
	url := "awskms://" + *key.KeyMetadata.KeyId + "?awssdk=v2"

	// Make a temporary role to assume
	iamClient := iam.NewFromConfig(cfg)
	roleName := "test-role-" + randomName(t)
	assumeRolePolicyDocument := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": {
			"Effect": "Allow",
			"Principal": { "AWS": "%s" },
			"Action": "sts:AssumeRole"
		}
	}`, *caller.Arn)
	role, err := iamClient.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName:                 &roleName,
		AssumeRolePolicyDocument: &assumeRolePolicyDocument,
	})
	require.NoError(t, err)
	defer func() {
		_, err := iamClient.DeleteRole(ctx, &iam.DeleteRoleInput{
			RoleName: &roleName,
		})
		assert.NoError(t, err)
	}()

	policyName := "test-policy-" + randomName(t)
	policyDocument := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": {
			"Effect": "Allow",
			"Action": [
				"kms:Encrypt",
				"kms:Decrypt"
			],
			"Resource": "%s"
		}
	}`, *key.KeyMetadata.Arn)
	policy, err := iamClient.CreatePolicy(ctx, &iam.CreatePolicyInput{
		PolicyName:     &policyName,
		PolicyDocument: &policyDocument,
	})
	require.NoError(t, err)
	defer func() {
		_, err := iamClient.DetachRolePolicy(ctx, &iam.DetachRolePolicyInput{
			PolicyArn: policy.Policy.Arn,
			RoleName:  &roleName,
		})
		assert.NoError(t, err)
		_, err = iamClient.DeletePolicy(ctx, &iam.DeletePolicyInput{
			PolicyArn: policy.Policy.Arn,
		})
		assert.NoError(t, err)
	}()
	_, err = iamClient.AttachRolePolicy(ctx, &iam.AttachRolePolicyInput{
		PolicyArn: policy.Policy.Arn,
		RoleName:  &roleName,
	})
	require.NoError(t, err)

	// AssumeRole takes about 10 seconds to take effect.
	// We'll try for up to 20.
	const (
		MaxAttempts = 10
		Delay       = 2 * time.Second
	)

	// Now assume that role and try and use the secret manager
	stsClient := sts.NewFromConfig(cfg)
	var assume *sts.AssumeRoleOutput
	for i := 0; i < MaxAttempts; i++ {
		sessionName := "test-session-" + randomName(t)
		assume, err = stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
			RoleArn:         role.Role.Arn,
			RoleSessionName: &sessionName,
		})
		if err == nil {
			break
		}
		assume = nil
		t.Logf("AssumeRole failed: %v", err)
		time.Sleep(Delay)
	}
	require.NotNil(t, assume, "Could not AssumeRole after %d attempts", MaxAttempts)

	creds := assume.Credentials
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", *creds.AccessKeyId)
	t.Setenv("AWS_SECRET_ACCESS_KEY", *creds.SecretAccessKey)
	t.Setenv("AWS_SESSION_TOKEN", *creds.SessionToken)

	testURL(ctx, t, url)
}

//nolint:paralleltest
func TestSecretsProviderOverride(t *testing.T) {
	// Don't call t.Parallel because we temporarily modify
	// PULUMI_CLOUD_SECRET_OVERRIDE env var and it may interfere with other
	// tests.

	stackConfig := &workspace.ProjectStack{}

	opener := &mockSecretsKeeperOpener{}
	secrets.DefaultURLMux().RegisterKeeper("test", opener)

	//nolint:paralleltest
	t.Run("without override", func(t *testing.T) {
		opener.wantURL = "test://foo"
		_, createSecretsManagerError := NewCloudSecretsManager(stackConfig, "test://foo", false)
		assert.Nil(t, createSecretsManagerError, "Creating the cloud secret manager should succeed")

		_, createSecretsManagerError = NewCloudSecretsManager(stackConfig, "test://bar", false)
		msg := "NewCloudSecretsManager with unexpected secretsProvider URL succeeded, expected an error"
		assert.NotNil(t, createSecretsManagerError, msg)
	})

	//nolint:paralleltest
	t.Run("with override", func(t *testing.T) {
		opener.wantURL = "test://bar"
		t.Setenv("PULUMI_CLOUD_SECRET_OVERRIDE", "test://bar")

		// Last argument here shouldn't matter anymore, since it gets overridden
		// by the env var. Both calls should succeed.
		msg := "creating the secrets manager should succeed regardless of secrets provider"
		_, createSecretsManagerError := NewCloudSecretsManager(stackConfig, "test://foo", false)
		assert.Nil(t, createSecretsManagerError, msg)
		_, createSecretsManagerError = NewCloudSecretsManager(stackConfig, "test://bar", false)
		assert.Nil(t, createSecretsManagerError, msg)
	})
}

type mockSecretsKeeperOpener struct {
	wantURL string
}

func (m *mockSecretsKeeperOpener) OpenKeeperURL(ctx context.Context, u *url.URL) (*secrets.Keeper, error) {
	if m.wantURL != u.String() {
		return nil, fmt.Errorf("got keeper URL: %q, want: %q", u, m.wantURL)
	}
	return secrets.NewKeeper(dummySecretsKeeper{}), nil
}

type dummySecretsKeeper struct {
	driver.Keeper
}

func (k dummySecretsKeeper) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	return ciphertext, nil
}

func (k dummySecretsKeeper) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	return plaintext, nil
}
