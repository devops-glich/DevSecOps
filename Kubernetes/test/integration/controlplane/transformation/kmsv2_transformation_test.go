//go:build !windows
// +build !windows

/*
Copyright 2022 The Kubernetes Authors.

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

package transformation

import (
	"bytes"
	"context"
	"crypto/aes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	clientv3 "go.etcd.io/etcd/client/v3"

	corev1 "k8s.io/api/core/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/server/options/encryptionconfig"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/storage/value"
	aestransformer "k8s.io/apiserver/pkg/storage/value/encrypt/aes"
	"k8s.io/apiserver/pkg/storage/value/encrypt/envelope/kmsv2"
	kmstypes "k8s.io/apiserver/pkg/storage/value/encrypt/envelope/kmsv2/v2"
	kmsv2mock "k8s.io/apiserver/pkg/storage/value/encrypt/envelope/testing/v2"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	kmsv2api "k8s.io/kms/apis/v2"
	kmsv2svc "k8s.io/kms/pkg/service"
	"k8s.io/kubernetes/test/integration"
	"k8s.io/kubernetes/test/integration/etcd"
)

type envelopekmsv2 struct {
	providerName string
	rawEnvelope  []byte
	plainTextDEK []byte
}

func (r envelopekmsv2) prefix() string {
	return fmt.Sprintf("k8s:enc:kms:v2:%s:", r.providerName)
}

func (r envelopekmsv2) prefixLen() int {
	return len(r.prefix())
}

func (r envelopekmsv2) cipherTextDEK() ([]byte, error) {
	o := &kmstypes.EncryptedObject{}
	if err := proto.Unmarshal(r.rawEnvelope[r.startOfPayload(r.providerName):], o); err != nil {
		return nil, err
	}
	return o.EncryptedDEK, nil
}

func (r envelopekmsv2) startOfPayload(_ string) int {
	return r.prefixLen()
}

func (r envelopekmsv2) cipherTextPayload() ([]byte, error) {
	o := &kmstypes.EncryptedObject{}
	if err := proto.Unmarshal(r.rawEnvelope[r.startOfPayload(r.providerName):], o); err != nil {
		return nil, err
	}
	return o.EncryptedData, nil
}

func (r envelopekmsv2) plainTextPayload(secretETCDPath string) ([]byte, error) {
	block, err := aes.NewCipher(r.plainTextDEK)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize AES Cipher: %v", err)
	}
	ctx := context.Background()
	dataCtx := value.DefaultContext(secretETCDPath)
	aesgcmTransformer, err := aestransformer.NewGCMTransformer(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create transformer from block: %v", err)
	}
	data, err := r.cipherTextPayload()
	if err != nil {
		return nil, fmt.Errorf("failed to get cipher text payload: %v", err)
	}
	plainSecret, _, err := aesgcmTransformer.TransformFromStorage(ctx, data, dataCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to transform from storage via AESGCM, err: %w", err)
	}

	return plainSecret, nil
}

// TestKMSv2Provider is an integration test between KubeAPI, ETCD and KMSv2 Plugin
// Concretely, this test verifies the following integration contracts:
// 1. Raw records in ETCD that were processed by KMSv2 Provider should be prefixed with k8s:enc:kms:v2:<plugin name>:
// 2. Data Encryption Key (DEK) should be generated by envelopeTransformer and passed to KMS gRPC Plugin
// 3. KMS gRPC Plugin should encrypt the DEK with a Key Encryption Key (KEK) and pass it back to envelopeTransformer
// 4. The cipherTextPayload (ex. Secret) should be encrypted via AES GCM transform
// 5. kmstypes.EncryptedObject structure should be serialized and deposited in ETCD
func TestKMSv2Provider(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KMSv2, true)()

	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       apiVersion: v2
       name: kms-provider
       endpoint: unix:///@kms-provider.sock
`

	providerName := "kms-provider"
	pluginMock, err := kmsv2mock.NewBase64Plugin("@kms-provider.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMSv2 Plugin: %v", err)
	}

	go pluginMock.Start()
	if err := kmsv2mock.WaitForBase64PluginToBeUp(pluginMock); err != nil {
		t.Fatalf("Failed start plugin, err: %v", err)
	}
	defer pluginMock.CleanUp()

	test, err := newTransformTest(t, encryptionConfig, false, "")
	if err != nil {
		t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
	}
	defer test.cleanUp()

	test.secret, err = test.createSecret(testSecret, testNamespace)
	if err != nil {
		t.Fatalf("Failed to create test secret, error: %v", err)
	}

	// Since Data Encryption Key (DEK) is randomly generated (per encryption operation), we need to ask KMS Mock for it.
	plainTextDEK := pluginMock.LastEncryptRequest()

	secretETCDPath := test.getETCDPathForResource(test.storageConfig.Prefix, "", "secrets", test.secret.Name, test.secret.Namespace)
	rawEnvelope, err := test.getRawSecretFromETCD()
	if err != nil {
		t.Fatalf("failed to read %s from etcd: %v", secretETCDPath, err)
	}

	envelopeData := envelopekmsv2{
		providerName: providerName,
		rawEnvelope:  rawEnvelope,
		plainTextDEK: plainTextDEK,
	}

	wantPrefix := envelopeData.prefix()
	if !bytes.HasPrefix(rawEnvelope, []byte(wantPrefix)) {
		t.Fatalf("expected secret to be prefixed with %s, but got %s", wantPrefix, rawEnvelope)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ciphertext, err := envelopeData.cipherTextDEK()
	if err != nil {
		t.Fatalf("failed to get ciphertext DEK from KMSv2 Plugin: %v", err)
	}
	decryptResponse, err := pluginMock.Decrypt(ctx, &kmsv2api.DecryptRequest{Uid: string(uuid.NewUUID()), Ciphertext: ciphertext})
	if err != nil {
		t.Fatalf("failed to decrypt DEK, %v", err)
	}
	dekPlainAsWouldBeSeenByETCD := decryptResponse.Plaintext

	if !bytes.Equal(plainTextDEK, dekPlainAsWouldBeSeenByETCD) {
		t.Fatalf("expected plainTextDEK %v to be passed to KMS Plugin, but got %s",
			plainTextDEK, dekPlainAsWouldBeSeenByETCD)
	}

	plainSecret, err := envelopeData.plainTextPayload(secretETCDPath)
	if err != nil {
		t.Fatalf("failed to transform from storage via AESGCM, err: %v", err)
	}

	if !strings.Contains(string(plainSecret), secretVal) {
		t.Fatalf("expected %q after decryption, but got %q", secretVal, string(plainSecret))
	}

	secretClient := test.restClient.CoreV1().Secrets(testNamespace)
	// Secrets should be un-enveloped on direct reads from Kube API Server.
	s, err := secretClient.Get(ctx, testSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get Secret from %s, err: %v", testNamespace, err)
	}
	if secretVal != string(s.Data[secretKey]) {
		t.Fatalf("expected %s from KubeAPI, but got %s", secretVal, string(s.Data[secretKey]))
	}
}

// TestKMSv2ProviderKeyIDStaleness is an integration test between KubeAPI and KMSv2 Plugin
// Concretely, this test verifies the following contracts for no-op updates:
// 1. When the key ID is unchanged, the resource version must not change
// 2. When the key ID changes, the resource version changes (but only once)
// 3. For all subsequent updates, the resource version must not change
// 4. When kms-plugin is down, expect creation of new pod and encryption to succeed while the DEK is valid
// 5. when kms-plugin is down, no-op update for a pod should succeed and not result in RV change while the DEK is valid
// 6. When kms-plugin is down, expect creation of new pod and encryption to fail once the DEK is invalid
// 7. when kms-plugin is down, no-op update for a pod should succeed and not result in RV change even once the DEK is valid
func TestKMSv2ProviderKeyIDStaleness(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KMSv2, true)()

	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - pods
    providers:
    - kms:
       apiVersion: v2
       name: kms-provider
       endpoint: unix:///@kms-provider.sock
`
	pluginMock, err := kmsv2mock.NewBase64Plugin("@kms-provider.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMSv2 Plugin: %v", err)
	}

	go pluginMock.Start()
	if err := kmsv2mock.WaitForBase64PluginToBeUp(pluginMock); err != nil {
		t.Fatalf("Failed start plugin, err: %v", err)
	}
	defer pluginMock.CleanUp()

	test, err := newTransformTest(t, encryptionConfig, false, "")
	if err != nil {
		t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
	}
	defer test.cleanUp()

	dynamicClient := dynamic.NewForConfigOrDie(test.kubeAPIServer.ClientConfig)

	testPod, err := test.createPod(testNamespace, dynamicClient)
	if err != nil {
		t.Fatalf("Failed to create test pod, error: %v, ns: %s", err, testNamespace)
	}
	version1 := testPod.GetResourceVersion()

	// 1. no-op update for the test pod should not result in any RV change
	updatedPod, err := test.inplaceUpdatePod(testNamespace, testPod, dynamicClient)
	if err != nil {
		t.Fatalf("Failed to update test pod, error: %v, ns: %s", err, testNamespace)
	}
	version2 := updatedPod.GetResourceVersion()
	if version1 != version2 {
		t.Fatalf("Resource version should not have changed. old pod: %v, new pod: %v", testPod, updatedPod)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	var firstEncryptedDEK []byte
	assertPodDEKs(ctx, t, test.kubeAPIServer.ServerOpts.Etcd.StorageConfig,
		1, 1,
		"k8s:enc:kms:v2:kms-provider:",
		func(_ int, counter uint64, etcdKey string, obj kmstypes.EncryptedObject) {
			firstEncryptedDEK = obj.EncryptedDEK

			if obj.KeyID != "1" {
				t.Errorf("key %s: want key ID %s, got %s", etcdKey, "1", obj.KeyID)
			}

			// with the first key we perform encryption during the following steps:
			// - create
			const want = 1_000_000_000 + 1 // zero value of counter is one billion
			if want != counter {
				t.Errorf("key %s: counter nonce is invalid: want %d, got %d", etcdKey, want, counter)
			}
		},
	)

	// 2. no-op update for the test pod with keyID update should result in RV change
	pluginMock.UpdateKeyID()
	if err := kmsv2mock.WaitForBase64PluginToBeUpdated(pluginMock); err != nil {
		t.Fatalf("Failed to update keyID for plugin, err: %v", err)
	}
	// Wait 1 sec (poll interval to check resource version) until a resource version change is detected or timeout at 1 minute.

	version3 := ""
	err = wait.Poll(time.Second, time.Minute,
		func() (bool, error) {
			t.Log("polling for in-place update rv change")
			updatedPod, err = test.inplaceUpdatePod(testNamespace, updatedPod, dynamicClient)
			if err != nil {
				return false, err
			}
			version3 = updatedPod.GetResourceVersion()
			if version1 != version3 {
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("Failed to detect one resource version update within the allotted time after keyID is updated and pod has been inplace updated, err: %v, ns: %s", err, testNamespace)
	}

	if version1 == version3 {
		t.Fatalf("Resource version should have changed after keyID update. old pod: %v, new pod: %v", testPod, updatedPod)
	}

	var wantCount uint64 = 1_000_000_000 // zero value of counter is one billion
	wantCount++                          // in place update with RV change

	// with the second key we perform encryption during the following steps:
	// - in place update with RV change
	// - delete (which does an update to set deletion timestamp)
	// - create
	checkDEK := func(_ int, counter uint64, etcdKey string, obj kmstypes.EncryptedObject) {
		if bytes.Equal(obj.EncryptedDEK, firstEncryptedDEK) {
			t.Errorf("key %s: incorrectly has the same EDEK", etcdKey)
		}

		if obj.KeyID != "2" {
			t.Errorf("key %s: want key ID %s, got %s", etcdKey, "2", obj.KeyID)
		}

		if wantCount != counter {
			t.Errorf("key %s: counter nonce is invalid: want %d, got %d", etcdKey, wantCount, counter)
		}
	}

	// 3. no-op update for the updated pod should not result in RV change
	updatedPod, err = test.inplaceUpdatePod(testNamespace, updatedPod, dynamicClient)
	if err != nil {
		t.Fatalf("Failed to update test pod, error: %v, ns: %s", err, testNamespace)
	}
	version4 := updatedPod.GetResourceVersion()
	if version3 != version4 {
		t.Fatalf("Resource version should not have changed again after the initial version updated as a result of the keyID update. old pod: %v, new pod: %v", testPod, updatedPod)
	}

	// delete the pod so that it can be recreated
	if err := test.deletePod(testNamespace, dynamicClient); err != nil {
		t.Fatalf("failed to delete test pod: %v", err)
	}
	wantCount++ // we cannot assert against the counter being 2 since the pod gets deleted

	// 4. when kms-plugin is down, expect creation of new pod and encryption to succeed because the DEK is still valid
	pluginMock.EnterFailedState()
	mustBeUnHealthy(t, "/kms-providers",
		"internal server error: kms-provider-0: rpc error: code = FailedPrecondition desc = failed precondition - key disabled",
		test.kubeAPIServer.ClientConfig)

	newPod, err := test.createPod(testNamespace, dynamicClient)
	if err != nil {
		t.Fatalf("Create test pod should have succeeded due to valid DEK, ns: %s, got: %v", testNamespace, err)
	}
	wantCount++
	version5 := newPod.GetResourceVersion()

	// 5. when kms-plugin is down and DEK is valid, no-op update for a pod should succeed and not result in RV change
	updatedPod, err = test.inplaceUpdatePod(testNamespace, newPod, dynamicClient)
	if err != nil {
		t.Fatalf("Failed to perform no-op update on pod when kms-plugin is down, error: %v, ns: %s", err, testNamespace)
	}
	version6 := updatedPod.GetResourceVersion()
	if version5 != version6 {
		t.Fatalf("Resource version should not have changed again after the initial version updated as a result of the keyID update. old pod: %v, new pod: %v", newPod, updatedPod)
	}

	// Invalidate the DEK by moving the current time forward
	origNowFunc := kmsv2.NowFunc
	t.Cleanup(func() { kmsv2.NowFunc = origNowFunc })
	kmsv2.NowFunc = func() time.Time { return origNowFunc().Add(5 * time.Minute) }

	// 6. when kms-plugin is down, expect creation of new pod and encryption to fail because the DEK is invalid
	_, err = test.createPod(testNamespace, dynamicClient)
	if err == nil || !strings.Contains(err.Error(), `EDEK with keyID "2" expired at 2`) {
		t.Fatalf("Create test pod should have failed due to encryption, ns: %s, got: %v", testNamespace, err)
	}

	// 7. when kms-plugin is down and DEK is invalid, no-op update for a pod should succeed and not result in RV change
	updatedNewPod, err := test.inplaceUpdatePod(testNamespace, newPod, dynamicClient)
	if err != nil {
		t.Fatalf("Failed to perform no-op update on pod when kms-plugin is down, error: %v, ns: %s", err, testNamespace)
	}
	version7 := updatedNewPod.GetResourceVersion()
	if version5 != version7 {
		t.Fatalf("Resource version should not have changed again after the initial version updated as a result of the keyID update. old pod: %v, new pod: %v", newPod, updatedNewPod)
	}

	assertPodDEKs(ctx, t, test.kubeAPIServer.ServerOpts.Etcd.StorageConfig,
		1, 1, "k8s:enc:kms:v2:kms-provider:", checkDEK,
	)
}

func TestKMSv2ProviderDEKReuse(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KMSv2, true)()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - pods
    providers:
    - kms:
       apiVersion: v2
       name: kms-provider
       endpoint: unix:///@kms-provider.sock
`
	pluginMock, err := kmsv2mock.NewBase64Plugin("@kms-provider.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMSv2 Plugin: %v", err)
	}

	go pluginMock.Start()
	if err := kmsv2mock.WaitForBase64PluginToBeUp(pluginMock); err != nil {
		t.Fatalf("Failed start plugin, err: %v", err)
	}
	defer pluginMock.CleanUp()

	test, err := newTransformTest(t, encryptionConfig, false, "")
	if err != nil {
		t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
	}
	t.Cleanup(test.cleanUp)

	client := kubernetes.NewForConfigOrDie(test.kubeAPIServer.ClientConfig)

	const podCount = 1_000

	for i := 0; i < podCount; i++ {
		if _, err := client.CoreV1().Pods(testNamespace).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("dek-reuse-%04d", i+1), // making creation order match returned list order / nonce counter
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "busybox",
						Image: "busybox",
					},
				},
			},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	assertPodDEKs(ctx, t, test.kubeAPIServer.ServerOpts.Etcd.StorageConfig,
		podCount, 1, // key ID does not change during the test so we should only have a single DEK
		"k8s:enc:kms:v2:kms-provider:",
		func(i int, counter uint64, etcdKey string, obj kmstypes.EncryptedObject) {
			if obj.KeyID != "1" {
				t.Errorf("key %s: want key ID %s, got %s", etcdKey, "1", obj.KeyID)
			}

			// zero value of counter is one billion so the first value will be one billion plus one
			// hence we add that to our zero based index to calculate the expected nonce
			if uint64(i+1_000_000_000+1) != counter {
				t.Errorf("key %s: counter nonce is invalid: want %d, got %d", etcdKey, i+1, counter)
			}
		},
	)
}

func assertPodDEKs(ctx context.Context, t *testing.T, config storagebackend.Config, podCount, dekCount int, kmsPrefix string,
	f func(i int, counter uint64, etcdKey string, obj kmstypes.EncryptedObject)) {
	t.Helper()

	rawClient, etcdClient, err := integration.GetEtcdClients(config.Transport)
	if err != nil {
		t.Fatalf("failed to create etcd client: %v", err)
	}
	t.Cleanup(func() { _ = rawClient.Close() })

	response, err := etcdClient.Get(ctx, "/"+config.Prefix+"/pods/"+testNamespace+"/", clientv3.WithPrefix())
	if err != nil {
		t.Fatal(err)
	}

	if len(response.Kvs) != podCount {
		t.Fatalf("expected %d KVs, but got %d", podCount, len(response.Kvs))
	}

	out := make([]kmstypes.EncryptedObject, len(response.Kvs))
	for i, kv := range response.Kvs {
		v := bytes.TrimPrefix(kv.Value, []byte(kmsPrefix))
		if err := proto.Unmarshal(v, &out[i]); err != nil {
			t.Fatal(err)
		}

		nonce := out[i].EncryptedData[:12]
		randN := nonce[:4]
		count := nonce[4:]

		if bytes.Equal(randN, make([]byte, len(randN))) {
			t.Errorf("key %s: got all zeros for first four bytes", string(kv.Key))
		}

		counter := binary.LittleEndian.Uint64(count)
		f(i, counter, string(kv.Key), out[i])
	}

	uniqueDEKs := sets.NewString()
	for _, object := range out {
		uniqueDEKs.Insert(string(object.EncryptedDEK))
	}

	if uniqueDEKs.Len() != dekCount {
		t.Errorf("expected %d DEKs, got: %d", dekCount, uniqueDEKs.Len())
	}
}

func TestKMSv2Healthz(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KMSv2, true)()

	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - secrets
    providers:
    - kms:
       apiVersion: v2
       name: provider-1
       endpoint: unix:///@kms-provider-1.sock
    - kms:
       apiVersion: v2
       name: provider-2
       endpoint: unix:///@kms-provider-2.sock
`

	pluginMock1, err := kmsv2mock.NewBase64Plugin("@kms-provider-1.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMS Plugin #1: %v", err)
	}

	if err := pluginMock1.Start(); err != nil {
		t.Fatalf("Failed to start kms-plugin, err: %v", err)
	}
	defer pluginMock1.CleanUp()
	if err := kmsv2mock.WaitForBase64PluginToBeUp(pluginMock1); err != nil {
		t.Fatalf("Failed to start plugin #1, err: %v", err)
	}

	pluginMock2, err := kmsv2mock.NewBase64Plugin("@kms-provider-2.sock")
	if err != nil {
		t.Fatalf("Failed to create mock of KMS Plugin #2: err: %v", err)
	}
	if err := pluginMock2.Start(); err != nil {
		t.Fatalf("Failed to start kms-plugin, err: %v", err)
	}
	defer pluginMock2.CleanUp()
	if err := kmsv2mock.WaitForBase64PluginToBeUp(pluginMock2); err != nil {
		t.Fatalf("Failed to start KMS Plugin #2: err: %v", err)
	}

	test, err := newTransformTest(t, encryptionConfig, false, "")
	if err != nil {
		t.Fatalf("Failed to start kube-apiserver, error: %v", err)
	}
	defer test.cleanUp()

	// Name of the healthz check is always "kms-provider-0" and it covers all kms plugins.

	// Stage 1 - Since all kms-plugins are guaranteed to be up,
	// the healthz check should be OK.
	mustBeHealthy(t, "/kms-providers", "ok", test.kubeAPIServer.ClientConfig)

	// Stage 2 - kms-plugin for provider-1 is down. Therefore, expect the healthz check
	// to fail and report that provider-1 is down
	pluginMock1.EnterFailedState()
	mustBeUnHealthy(t, "/kms-providers",
		"internal server error: kms-provider-0: rpc error: code = FailedPrecondition desc = failed precondition - key disabled",
		test.kubeAPIServer.ClientConfig)
	pluginMock1.ExitFailedState()

	// Stage 3 - kms-plugin for provider-1 is now up. Therefore, expect the health check for provider-1
	// to succeed now, but provider-2 is now down.
	pluginMock2.EnterFailedState()
	mustBeUnHealthy(t, "/kms-providers",
		"internal server error: kms-provider-1: rpc error: code = FailedPrecondition desc = failed precondition - key disabled",
		test.kubeAPIServer.ClientConfig)
	pluginMock2.ExitFailedState()

	// Stage 4 - All kms-plugins are once again up,
	// the healthz check should be OK.
	mustBeHealthy(t, "/kms-providers", "ok", test.kubeAPIServer.ClientConfig)

	// Stage 5 - All kms-plugins are unhealthy at the same time and we can observe both failures.
	pluginMock1.EnterFailedState()
	pluginMock2.EnterFailedState()
	mustBeUnHealthy(t, "/kms-providers",
		"internal server error: "+
			"[kms-provider-0: failed to perform status section of the healthz check for KMS Provider provider-1, error: rpc error: code = FailedPrecondition desc = failed precondition - key disabled,"+
			" kms-provider-1: failed to perform status section of the healthz check for KMS Provider provider-2, error: rpc error: code = FailedPrecondition desc = failed precondition - key disabled]",
		test.kubeAPIServer.ClientConfig)
}

func TestKMSv2SingleService(t *testing.T) {
	defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KMSv2, true)()

	var kmsv2Calls int
	origEnvelopeKMSv2ServiceFactory := encryptionconfig.EnvelopeKMSv2ServiceFactory
	encryptionconfig.EnvelopeKMSv2ServiceFactory = func(ctx context.Context, endpoint, providerName string, callTimeout time.Duration) (kmsv2svc.Service, error) {
		kmsv2Calls++
		return origEnvelopeKMSv2ServiceFactory(ctx, endpoint, providerName, callTimeout)
	}
	t.Cleanup(func() {
		encryptionconfig.EnvelopeKMSv2ServiceFactory = origEnvelopeKMSv2ServiceFactory
	})

	// check resources provided by the three servers that we have wired together
	// - pods and config maps from KAS
	// - CRDs and CRs from API extensions
	// - API services from aggregator
	encryptionConfig := `
kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
    - pods
    - configmaps
    - customresourcedefinitions.apiextensions.k8s.io
    - pandas.awesome.bears.com
    - apiservices.apiregistration.k8s.io
    providers:
    - kms:
       apiVersion: v2
       name: kms-provider
       endpoint: unix:///@kms-provider.sock
`

	pluginMock, err := kmsv2mock.NewBase64Plugin("@kms-provider.sock")
	if err != nil {
		t.Fatalf("failed to create mock of KMSv2 Plugin: %v", err)
	}

	go pluginMock.Start()
	if err := kmsv2mock.WaitForBase64PluginToBeUp(pluginMock); err != nil {
		t.Fatalf("Failed start plugin, err: %v", err)
	}
	t.Cleanup(pluginMock.CleanUp)

	test, err := newTransformTest(t, encryptionConfig, false, "")
	if err != nil {
		t.Fatalf("failed to start KUBE API Server with encryptionConfig\n %s, error: %v", encryptionConfig, err)
	}
	t.Cleanup(test.cleanUp)

	// the storage registry for CRs is dynamic so create one to exercise the wiring
	etcd.CreateTestCRDs(t, apiextensionsclientset.NewForConfigOrDie(test.kubeAPIServer.ClientConfig), false, etcd.GetCustomResourceDefinitionData()...)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	gvr := schema.GroupVersionResource{Group: "awesome.bears.com", Version: "v1", Resource: "pandas"}
	stub := etcd.GetEtcdStorageData()[gvr].Stub
	dynamicClient, obj, err := etcd.JSONToUnstructured(stub, "", &meta.RESTMapping{
		Resource:         gvr,
		GroupVersionKind: gvr.GroupVersion().WithKind("Panda"),
		Scope:            meta.RESTScopeRoot,
	}, dynamic.NewForConfigOrDie(test.kubeAPIServer.ClientConfig))
	if err != nil {
		t.Fatal(err)
	}
	_, err = dynamicClient.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if kmsv2Calls != 1 {
		t.Fatalf("expected a single call to KMS v2 service factory: %v", kmsv2Calls)
	}
}