package watchlist

import (
	"errors"
	"strings"

	"github.com/gostdlib/base/context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// WithBookmarkConfigMap configures bookmark persistence in a caller-provisioned ConfigMap.
// The ConfigMap must already exist; this option will not create it.
func WithBookmarkConfigMap(namespace, name string) Option {
	return func(c *Reader) error {
		namespace = strings.TrimSpace(namespace)
		name = strings.TrimSpace(name)
		if namespace == "" {
			return errors.New("bookmark namespace is empty")
		}
		if name == "" {
			return errors.New("bookmark ConfigMap name is empty")
		}
		c.bookmarkStore = newConfigMapBookmarkStore(c.clientset, namespace, name)
		return nil
	}
}

type configMapBookmarkStore struct {
	clientset kubernetes.Interface
	namespace string
	name      string
}

func newConfigMapBookmarkStore(clientset kubernetes.Interface, namespace, name string) *configMapBookmarkStore {
	return &configMapBookmarkStore{
		clientset: clientset,
		namespace: namespace,
		name:      name,
	}
}

func (s *configMapBookmarkStore) Load(ctx context.Context, gvr schema.GroupVersionResource) (string, error) {
	cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return cm.Data[bookmarkConfigMapDataKey(gvr)], nil
}

func (s *configMapBookmarkStore) Store(ctx context.Context, gvr schema.GroupVersionResource, resourceVersion string) error {
	key := bookmarkConfigMapDataKey(gvr)
	if key == "" || resourceVersion == "" {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := s.clientset.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		cm = cm.DeepCopy()
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if cm.Data[key] == resourceVersion {
			return nil
		}
		cm.Data[key] = resourceVersion
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

func (s *configMapBookmarkStore) Delete(ctx context.Context, gvr schema.GroupVersionResource) error {
	key := bookmarkConfigMapDataKey(gvr)
	if key == "" {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
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

		cm = cm.DeepCopy()
		delete(cm.Data, key)
		_, err = s.clientset.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

func bookmarkConfigMapDataKey(gvr schema.GroupVersionResource) string {
	return bookmarkResourceName(gvr)
}
