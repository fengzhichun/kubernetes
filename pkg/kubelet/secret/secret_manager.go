/*
Copyright 2016 The Kubernetes Authors.

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

package secret

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/kubernetes/pkg/api/v1"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
	storageetcd "k8s.io/kubernetes/pkg/storage/etcd"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/pkg/util/clock"
)

type Manager interface {
	// Get secret by secret namespace and name.
	GetSecret(namespace, name string) (*v1.Secret, error)

	// WARNING: Register/UnregisterPod functions should be efficient,
	// i.e. should not block on network operations.

	// RegisterPod registers all secrets from a given pod.
	RegisterPod(pod *v1.Pod)

	// UnregisterPod unregisters secrets from a given pod that are not
	// used by any other registered pod.
	UnregisterPod(pod *v1.Pod)
}

// simpleSecretManager implements SecretManager interfaces with
// simple operations to apiserver.
type simpleSecretManager struct {
	kubeClient clientset.Interface
}

func NewSimpleSecretManager(kubeClient clientset.Interface) (Manager, error) {
	return &simpleSecretManager{kubeClient: kubeClient}, nil
}

func (s *simpleSecretManager) GetSecret(namespace, name string) (*v1.Secret, error) {
	return s.kubeClient.Core().Secrets(namespace).Get(name, metav1.GetOptions{})
}

func (s *simpleSecretManager) RegisterPod(pod *v1.Pod) {
}

func (s *simpleSecretManager) UnregisterPod(pod *v1.Pod) {
}

type objectKey struct {
	namespace string
	name      string
}

// secretStoreItems is a single item stored in secretStore.
type secretStoreItem struct {
	refCount int
	secret   *secretData
}

type secretData struct {
	sync.Mutex

	secret         *v1.Secret
	err            error
	lastUpdateTime time.Time
}

// secretStore is a local cache of secrets.
type secretStore struct {
	kubeClient clientset.Interface
	clock      clock.Clock

	lock  sync.Mutex
	items map[objectKey]*secretStoreItem
	ttl   time.Duration
}

func newSecretStore(kubeClient clientset.Interface, clock clock.Clock, ttl time.Duration) *secretStore {
	return &secretStore{
		kubeClient: kubeClient,
		clock:      clock,
		items:      make(map[objectKey]*secretStoreItem),
		ttl:        ttl,
	}
}

func isSecretOlder(newSecret, oldSecret *v1.Secret) bool {
	newVersion, _ := storageetcd.Versioner.ObjectResourceVersion(newSecret)
	oldVersion, _ := storageetcd.Versioner.ObjectResourceVersion(oldSecret)
	return newVersion < oldVersion
}

func (s *secretStore) Add(namespace, name string) {
	key := objectKey{namespace: namespace, name: name}

	// Add is called from RegisterPod, thus it needs to be efficient.
	// Thus Add() is only increasing refCount and generation of a given secret.
	// Then Get() is responsible for fetching if needed.
	s.lock.Lock()
	defer s.lock.Unlock()
	item, exists := s.items[key]
	if !exists {
		item = &secretStoreItem{
			refCount: 0,
			secret:   &secretData{},
		}
		s.items[key] = item
	}

	item.refCount++
	// This will trigger fetch on the next Get() operation.
	item.secret = nil
}

func (s *secretStore) Delete(namespace, name string) {
	key := objectKey{namespace: namespace, name: name}

	s.lock.Lock()
	defer s.lock.Unlock()
	if item, ok := s.items[key]; ok {
		item.refCount--
		if item.refCount == 0 {
			delete(s.items, key)
		}
	}
}

func (s *secretStore) Get(namespace, name string) (*v1.Secret, error) {
	key := objectKey{namespace: namespace, name: name}

	data := func() *secretData {
		s.lock.Lock()
		defer s.lock.Unlock()
		item, exists := s.items[key]
		if !exists {
			return nil
		}
		if item.secret == nil {
			item.secret = &secretData{}
		}
		return item.secret
	}()
	if data == nil {
		return nil, fmt.Errorf("secret %q/%q not registered", namespace, name)
	}

	// After updating data in secretStore, lock the data, fetch secret if
	// needed and return data.
	data.Lock()
	defer data.Unlock()
	if data.err != nil || !s.clock.Now().Before(data.lastUpdateTime.Add(s.ttl)) {
		secret, err := s.kubeClient.Core().Secrets(namespace).Get(name, metav1.GetOptions{})
		// Update state, unless we got error different than "not-found".
		if err == nil || apierrors.IsNotFound(err) {
			// Ignore the update to the older version of a secret.
			if data.secret == nil || secret == nil || !isSecretOlder(secret, data.secret) {
				data.secret = secret
				data.err = err
				data.lastUpdateTime = s.clock.Now()
			}
		} else if data.secret == nil && data.err == nil {
			// We have unitialized secretData - return current result.
			return secret, err
		}
	}
	return data.secret, data.err
}

// cachingSecretManager keeps a cache of all secrets necessary for registered pods.
// It implements the following logic:
// - whenever a pod is created or updated, the cached versions of all its secrets
//   are invalidated
// - every GetSecret() call tries to fetch the value from local cache; if it is
//   not there, invalidated or too old, we fetch it from apiserver and refresh the
//   value in cache; otherwise it is just fetched from cache
type cachingSecretManager struct {
	secretStore *secretStore

	lock           sync.Mutex
	registeredPods map[objectKey]*v1.Pod
}

func NewCachingSecretManager(kubeClient clientset.Interface) (Manager, error) {
	csm := &cachingSecretManager{
		secretStore:    newSecretStore(kubeClient, clock.RealClock{}, time.Minute),
		registeredPods: make(map[objectKey]*v1.Pod),
	}
	return csm, nil
}

func (c *cachingSecretManager) GetSecret(namespace, name string) (*v1.Secret, error) {
	return c.secretStore.Get(namespace, name)
}

// TODO: Before we will use secretManager in other places (e.g. for secret volumes)
// we should update this function to also get secrets from those places.
func getSecretNames(pod *v1.Pod) sets.String {
	result := sets.NewString()
	for _, reference := range pod.Spec.ImagePullSecrets {
		result.Insert(reference.Name)
	}
	for i := range pod.Spec.Containers {
		for _, envVar := range pod.Spec.Containers[i].Env {
			if envVar.ValueFrom != nil && envVar.ValueFrom.SecretKeyRef != nil {
				result.Insert(envVar.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	return result
}

func (c *cachingSecretManager) RegisterPod(pod *v1.Pod) {
	names := getSecretNames(pod)
	c.lock.Lock()
	defer c.lock.Unlock()
	for name := range names {
		c.secretStore.Add(pod.Namespace, name)
	}
	var prev *v1.Pod
	key := objectKey{namespace: pod.Namespace, name: pod.Name}
	prev = c.registeredPods[key]
	c.registeredPods[key] = pod
	if prev != nil {
		for name := range getSecretNames(prev) {
			c.secretStore.Delete(prev.Namespace, name)
		}
	}
}

func (c *cachingSecretManager) UnregisterPod(pod *v1.Pod) {
	var prev *v1.Pod
	key := objectKey{namespace: pod.Namespace, name: pod.Name}
	c.lock.Lock()
	defer c.lock.Unlock()
	prev = c.registeredPods[key]
	delete(c.registeredPods, key)
	if prev != nil {
		for name := range getSecretNames(prev) {
			c.secretStore.Delete(prev.Namespace, name)
		}
	}
}
