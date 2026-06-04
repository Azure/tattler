// Package bookmarks provides BookmarkStore implementations for persisting
// watch bookmark resourceVersions used by the watchlist Reader to resume on startup.
package bookmarks

import (
	"fmt"
	"strings"

	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/retry/exponential"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

var back = exponential.Must(exponential.New())

// maxAttempts is the number of attempts made against the APIServer before giving up.
const maxAttempts = 5

// ConfigMapStore persists watch bookmark resourceVersions in a caller-provisioned
// ConfigMap. The ConfigMap must already exist; ConfigMapStore will not create it.
type ConfigMapStore struct {
	clientset kubernetes.Interface
	namespace string
	name      string
}

// NewConfigMapStore returns a ConfigMapStore that reads and writes bookmark
// resourceVersions to the ConfigMap identified by namespace and name. The
// ConfigMap must already exist. It panics if clientset is nil or namespace or
// name is empty.
func NewConfigMapStore(clientset kubernetes.Interface, namespace, name string) *ConfigMapStore {
	namespace = strings.TrimSpace(namespace)
	name = strings.TrimSpace(name)

	if clientset == nil || namespace == "" || name == "" {
		panic("bookmarks.NewConfigMapStore: clientset must be non-nil and namespace and name must be non-empty")
	}

	return &ConfigMapStore{
		clientset: clientset,
		namespace: namespace,
		name:      name,
	}
}

// Load returns the stored resourceVersion for gvr, or an empty string if none is set.
func (s *ConfigMapStore) Load(ctx context.Context, gvr schema.GroupVersionResource) (string, error) {
	var rv string
	err := back.Retry(
		ctx,
		func(ctx context.Context, _ exponential.Record) error {
			cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				// The caller is responsible for provisioning the ConfigMap, so retrying cannot succeed.
				return fmt.Errorf("%w: %w", err, exponential.ErrPermanent)
			}
			if err != nil {
				return err
			}
			rv = cm.Data[cmKey(gvr)]
			return nil
		},
		exponential.WithMaxAttempts(maxAttempts),
	)
	if err != nil {
		return "", err
	}
	return rv, nil
}

// Store persists resourceVersion for gvr in the ConfigMap.
func (s *ConfigMapStore) Store(ctx context.Context, gvr schema.GroupVersionResource, resourceVersion string) error {
	key := cmKey(gvr)
	if key == "" || resourceVersion == "" {
		return nil
	}
	return back.Retry(
		ctx,
		func(ctx context.Context, _ exponential.Record) error {
			cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				// The caller is responsible for provisioning the ConfigMap, so retrying cannot succeed.
				return fmt.Errorf("%w: %w", err, exponential.ErrPermanent)
			}
			if err != nil {
				return err
			}

			if cm.Data == nil {
				cm.Data = map[string]string{key: resourceVersion}
			} else if cm.Data[key] == resourceVersion {
				return nil
			} else {
				cm.Data[key] = resourceVersion
			}

			_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
			return err
		},
		exponential.WithMaxAttempts(maxAttempts),
	)
}

// Delete removes the stored resourceVersion for gvr from the ConfigMap.
func (s *ConfigMapStore) Delete(ctx context.Context, gvr schema.GroupVersionResource) error {
	key := cmKey(gvr)
	if key == "" {
		return nil
	}
	return back.Retry(
		ctx,
		func(ctx context.Context, _ exponential.Record) error {
			cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if _, ok := cm.Data[key]; !ok {
				return nil
			}

			delete(cm.Data, key)
			_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
			return err
		},
		exponential.WithMaxAttempts(maxAttempts),
	)
}

// cmKey returns the ConfigMap data key used to store gvr's resourceVersion.
func cmKey(gvr schema.GroupVersionResource) string {
	if gvr.Empty() {
		return ""
	}
	group := gvr.Group
	if group == "" {
		// Core Kubernetes resources use an empty API group in GroupVersionResource.
		// .v1.pods vs core.v1.pods
		group = "core"
	}
	return fmt.Sprintf("%s.%s.%s", group, gvr.Version, gvr.Resource)
}
