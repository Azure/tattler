package configmap

import (
	"testing"

	"github.com/gostdlib/base/context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNewPanics(t *testing.T) {
	t.Parallel()

	clientset := fake.NewSimpleClientset()

	tests := []struct {
		name      string
		clientset kubernetes.Interface
		namespace string
		cmName    string
		wantPanic bool
	}{
		{name: "valid", clientset: clientset, namespace: "default", cmName: "tattler-bookmarks", wantPanic: false},
		{name: "nil clientset", clientset: nil, namespace: "default", cmName: "tattler-bookmarks", wantPanic: true},
		{name: "empty namespace", clientset: clientset, namespace: "  ", cmName: "tattler-bookmarks", wantPanic: true},
		{name: "empty name", clientset: clientset, namespace: "default", cmName: "  ", wantPanic: true},
	}

	for _, test := range tests {
		func() {
			defer func() {
				r := recover()
				if test.wantPanic && r == nil {
					t.Errorf("TestNewPanics(%s): got no panic, want panic", test.name)
				}
				if !test.wantPanic && r != nil {
					t.Errorf("TestNewPanics(%s): got panic %v, want no panic", test.name, r)
				}
			}()

			New(test.clientset, test.namespace, test.cmName)
		}()
	}
}

func TestStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	const namespace = "default"
	const name = "tattler-bookmarks"
	key := corev1.SchemeGroupVersion.WithResource("pods")
	dataKey := cmKey(key)

	t.Run("Load requires provisioned ConfigMap", func(t *testing.T) {
		t.Parallel()

		store := New(fake.NewSimpleClientset(), namespace, name)
		_, err := store.Load(ctx, key)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("Load() got err %v, want not found", err)
		}
	})

	t.Run("Store requires provisioned ConfigMap", func(t *testing.T) {
		t.Parallel()

		clientset := fake.NewSimpleClientset()
		store := New(clientset, namespace, name)
		err := store.Store(ctx, key, "100")
		if !apierrors.IsNotFound(err) {
			t.Fatalf("Store() got err %v, want not found", err)
		}
		_, err = clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("Get() got err %v, want not found", err)
		}
	})

	t.Run("Store updates provisioned ConfigMap", func(t *testing.T) {
		t.Parallel()

		clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data:       map[string]string{},
		})
		store := New(clientset, namespace, name)
		if err := store.Store(ctx, key, "100"); err != nil {
			t.Fatalf("Store() got err %v, want nil", err)
		}

		configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get() got err %v, want nil", err)
		}
		if got := configMap.Data[dataKey]; got != "100" {
			t.Fatalf("ConfigMap data[%q] got %q, want %q", dataKey, got, "100")
		}
	})

	t.Run("Delete removes key from provisioned ConfigMap", func(t *testing.T) {
		t.Parallel()

		clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data:       map[string]string{dataKey: "100"},
		})
		store := New(clientset, namespace, name)
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("Delete() got err %v, want nil", err)
		}

		configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get() got err %v, want nil", err)
		}
		if _, ok := configMap.Data[dataKey]; ok {
			t.Fatalf("ConfigMap data[%q] still present, want removed", dataKey)
		}
	})
}
