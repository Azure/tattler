// Package configmap provides a store.Bookmarks implementation backed by a Kubernetes ConfigMap.
package configmap

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store/internal/private"
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

// Store persists watch bookmark resourceVersions in a caller-provisioned ConfigMap.
type Store struct {
	clientset kubernetes.Interface
	namespace string
	name      string
}

// New returns a Store that reads and writes bookmark resourceVersions to an existing ConfigMap.
func New(clientset kubernetes.Interface, namespace, name string) (*Store, error) {
	namespace = strings.TrimSpace(namespace)
	name = strings.TrimSpace(name)

	if clientset == nil {
		return nil, errors.New("clientset is nil")
	}
	if namespace == "" {
		return nil, errors.New("namespace is empty")
	}
	if name == "" {
		return nil, errors.New("name is empty")
	}

	return &Store{
		clientset: clientset,
		namespace: namespace,
		name:      name,
	}, nil
}

// Load returns the stored resourceVersion for gvr, or an empty string if none is set.
func (cm *Store) Load(ctx context.Context, gvr schema.GroupVersionResource) (string, error) {
	var resourceVersion string
	err := back.Retry(
		ctx,
		func(ctx context.Context, _ exponential.Record) error {
			configMap, err := cm.clientset.CoreV1().ConfigMaps(cm.namespace).Get(ctx, cm.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				// The caller provisions the ConfigMap, so retrying cannot make a missing ConfigMap appear.
				return fmt.Errorf("%w: %w", err, exponential.ErrPermanent)
			}
			if err != nil {
				return err
			}
			resourceVersion = configMap.Data[cmKey(gvr)]
			return nil
		},
		exponential.WithMaxAttempts(maxAttempts),
	)
	if err != nil {
		return "", err
	}
	return resourceVersion, nil
}

// Store persists resourceVersion for gvr in the provisioned ConfigMap without changing other keys.
func (cm *Store) Store(ctx context.Context, gvr schema.GroupVersionResource, resourceVersion string) error {
	key := cmKey(gvr)
	if key == "" || resourceVersion == "" {
		return nil
	}
	return back.Retry(
		ctx,
		func(ctx context.Context, _ exponential.Record) error {
			configMap, err := cm.clientset.CoreV1().ConfigMaps(cm.namespace).Get(ctx, cm.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				// The caller provisions the ConfigMap, so retrying cannot make a missing ConfigMap appear.
				return fmt.Errorf("%w: %w", err, exponential.ErrPermanent)
			}
			if err != nil {
				return err
			}

			if configMap.Data == nil {
				configMap.Data = map[string]string{key: resourceVersion}
			} else if configMap.Data[key] == resourceVersion {
				return nil
			} else {
				configMap.Data[key] = resourceVersion
			}

			_, err = cm.clientset.CoreV1().ConfigMaps(cm.namespace).Update(ctx, configMap, metav1.UpdateOptions{})
			return err
		},
		exponential.WithMaxAttempts(maxAttempts),
	)
}

// Delete removes the stored resourceVersion for gvr from the ConfigMap and leaves all other keys intact.
func (cm *Store) Delete(ctx context.Context, gvr schema.GroupVersionResource) error {
	key := cmKey(gvr)
	if key == "" {
		return nil
	}
	return back.Retry(
		ctx,
		func(ctx context.Context, _ exponential.Record) error {
			configMap, err := cm.clientset.CoreV1().ConfigMaps(cm.namespace).Get(ctx, cm.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if _, ok := configMap.Data[key]; !ok {
				return nil
			}

			delete(configMap.Data, key)
			_, err = cm.clientset.CoreV1().ConfigMaps(cm.namespace).Update(ctx, configMap, metav1.UpdateOptions{})
			return err
		},
		exponential.WithMaxAttempts(maxAttempts),
	)
}

func (*Store) Package(private.Package) {}

func cmKey(gvr schema.GroupVersionResource) string {
	if gvr.Empty() {
		return ""
	}
	group := gvr.Group
	if group == "" {
		group = "core"
	}
	return fmt.Sprintf("%s.%s.%s", group, gvr.Version, gvr.Resource)
}
