package configmap

import (
	"errors"
	"testing"

	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/retry/exponential"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func init() {
	back = exponential.Must(exponential.New(exponential.WithTesting()))
}

const (
	testNamespace = "default"
	testName      = "tattler-bookmarks"
)

func TestNew(t *testing.T) {
	t.Parallel()

	clientset := fake.NewSimpleClientset()
	tests := []struct {
		name      string
		clientset kubernetes.Interface
		namespace string
		cmName    string
		options   []Option
		wantErr   bool
	}{
		{name: "valid", clientset: clientset, namespace: testNamespace, cmName: testName},
		{
			name:      "valid option",
			clientset: clientset,
			namespace: testNamespace,
			cmName:    testName,
			options: []Option{func(store *Store) error {
				if store.clientset != clientset || store.namespace != testNamespace || store.name != testName {
					return errors.New("option got uninitialized store")
				}
				return nil
			}},
		},
		{
			name:      "option error",
			clientset: clientset,
			namespace: testNamespace,
			cmName:    testName,
			options: []Option{func(*Store) error {
				return errors.New("option error")
			}},
			wantErr: true,
		},
		{name: "nil clientset", clientset: nil, namespace: testNamespace, cmName: testName, wantErr: true},
		{name: "empty namespace", clientset: clientset, namespace: "  ", cmName: testName, wantErr: true},
		{name: "empty name", clientset: clientset, namespace: testNamespace, cmName: "  ", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store, err := New(test.clientset, test.namespace, test.cmName, test.options...)
			if test.wantErr {
				if err == nil {
					t.Fatal("New() got nil err, want error")
				}
				if store != nil {
					t.Fatalf("New() got store %#v, want nil", store)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() got err %v, want nil", err)
			}
			if store == nil {
				t.Fatal("New() got nil store, want non-nil")
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	key := corev1.SchemeGroupVersion.WithResource("pods")
	dataKey := cmKey(key)

	tests := []struct {
		name    string
		objects []runtime.Object
		want    string
		wantErr func(error) bool
	}{
		{
			name:    "requires provisioned configmap",
			wantErr: apierrors.IsNotFound,
		},
		{
			name: "returns empty string for missing bookmark",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
				Data:       map[string]string{},
			}},
		},
		{
			name: "returns stored bookmark",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
				Data:       map[string]string{dataKey: "100"},
			}},
			want: "100",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			clientset := fake.NewSimpleClientset(test.objects...)
			store, err := New(clientset, testNamespace, testName)
			if err != nil {
				t.Fatalf("New() got err %v, want nil", err)
			}

			got, err := store.Load(ctx, key)
			if test.wantErr != nil {
				if !test.wantErr(err) {
					t.Fatalf("Load() got err %v, want matching error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() got err %v, want nil", err)
			}
			if got != test.want {
				t.Fatalf("Load() got %q, want %q", got, test.want)
			}
		})
	}
}

func TestStore(t *testing.T) {
	t.Parallel()

	nonNilErr := func(err error) bool { return err != nil }
	key := corev1.SchemeGroupVersion.WithResource("pods")
	dataKey := cmKey(key)
	otherKey := cmKey(corev1.SchemeGroupVersion.WithResource("services"))

	tests := []struct {
		name    string
		objects []runtime.Object
		gvr     schema.GroupVersionResource
		rv      string
		setup   func(*fake.Clientset)
		check   func(*testing.T, *fake.Clientset)
		wantErr func(error) bool
	}{
		{
			name:    "requires provisioned configmap",
			gvr:     key,
			rv:      "100",
			wantErr: apierrors.IsNotFound,
			check: func(t *testing.T, clientset *fake.Clientset) {
				_, err := clientset.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), testName, metav1.GetOptions{})
				if !apierrors.IsNotFound(err) {
					t.Fatalf("Get() got err %v, want not found", err)
				}
			},
		},
		{
			name: "errors on empty gvr",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
				Data:       map[string]string{},
			}},
			gvr:     schema.GroupVersionResource{},
			rv:      "100",
			wantErr: nonNilErr,
			check: func(t *testing.T, clientset *fake.Clientset) {
				configMap := getConfigMap(t, t.Context(), clientset, testNamespace, testName)
				if len(configMap.Data) != 0 {
					t.Fatalf("ConfigMap data got %#v, want empty", configMap.Data)
				}
			},
		},
		{
			name: "errors on empty resourceVersion",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
				Data:       map[string]string{},
			}},
			gvr:     key,
			rv:      "",
			wantErr: nonNilErr,
			check: func(t *testing.T, clientset *fake.Clientset) {
				configMap := getConfigMap(t, t.Context(), clientset, testNamespace, testName)
				if len(configMap.Data) != 0 {
					t.Fatalf("ConfigMap data got %#v, want empty", configMap.Data)
				}
			},
		},
		{
			name: "initializes data",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
			}},
			gvr: key,
			rv:  "100",
			check: func(t *testing.T, clientset *fake.Clientset) {
				configMap := getConfigMap(t, t.Context(), clientset, testNamespace, testName)
				if got := configMap.Data[dataKey]; got != "100" {
					t.Fatalf("ConfigMap data[%q] got %q, want %q", dataKey, got, "100")
				}
			},
		},
		{
			name: "preserves other bookmarks",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
				Data:       map[string]string{otherKey: "50"},
			}},
			gvr: key,
			rv:  "100",
			check: func(t *testing.T, clientset *fake.Clientset) {
				configMap := getConfigMap(t, t.Context(), clientset, testNamespace, testName)
				if got := configMap.Data[dataKey]; got != "100" {
					t.Fatalf("ConfigMap data[%q] got %q, want %q", dataKey, got, "100")
				}
				if got := configMap.Data[otherKey]; got != "50" {
					t.Fatalf("ConfigMap data[%q] got %q, want %q", otherKey, got, "50")
				}
			},
		},
		{
			name: "retries update conflicts",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
				Data:       map[string]string{},
			}},
			gvr: key,
			rv:  "100",
			setup: func(clientset *fake.Clientset) {
				attempts := 0
				clientset.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
					attempts++
					if attempts == 1 {
						return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, testName, errors.New("stale resource version"))
					}
					return false, nil, nil
				})
			},
			check: func(t *testing.T, clientset *fake.Clientset) {
				configMap := getConfigMap(t, t.Context(), clientset, testNamespace, testName)
				if got := configMap.Data[dataKey]; got != "100" {
					t.Fatalf("ConfigMap data[%q] got %q, want %q", dataKey, got, "100")
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			clientset := fake.NewSimpleClientset(test.objects...)
			if test.setup != nil {
				test.setup(clientset)
			}
			store, err := New(clientset, testNamespace, testName)
			if err != nil {
				t.Fatalf("New() got err %v, want nil", err)
			}

			err = store.Store(ctx, test.gvr, test.rv)
			if test.wantErr != nil {
				if !test.wantErr(err) {
					t.Fatalf("Store() got err %v, want matching error", err)
				}
			} else if err != nil {
				t.Fatalf("Store() got err %v, want nil", err)
			}
			if test.check != nil {
				test.check(t, clientset)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	key := corev1.SchemeGroupVersion.WithResource("pods")
	dataKey := cmKey(key)
	otherKey := cmKey(corev1.SchemeGroupVersion.WithResource("services"))

	tests := []struct {
		name    string
		objects []runtime.Object
		gvr     schema.GroupVersionResource
		rv      string
		wantErr error
		check   func(*testing.T, *fake.Clientset)
	}{
		{
			name: "ignores missing configmap",
			gvr:  key,
			rv:   "100",
		},
		{
			name: "ignores empty gvr",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
				Data:       map[string]string{dataKey: "100"},
			}},
			gvr: schema.GroupVersionResource{},
			rv:  "100",
			check: func(t *testing.T, clientset *fake.Clientset) {
				configMap := getConfigMap(t, t.Context(), clientset, testNamespace, testName)
				if got := configMap.Data[dataKey]; got != "100" {
					t.Fatalf("ConfigMap data[%q] got %q, want %q", dataKey, got, "100")
				}
			},
		},
		{
			name: "ignores missing bookmark",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
				Data:       map[string]string{otherKey: "50"},
			}},
			gvr: key,
			rv:  "100",
			check: func(t *testing.T, clientset *fake.Clientset) {
				configMap := getConfigMap(t, t.Context(), clientset, testNamespace, testName)
				if got := configMap.Data[otherKey]; got != "50" {
					t.Fatalf("ConfigMap data[%q] got %q, want %q", otherKey, got, "50")
				}
			},
		},
		{
			name: "preserves replaced bookmark",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
				Data:       map[string]string{dataKey: "500", otherKey: "50"},
			}},
			gvr:     key,
			rv:      "100",
			wantErr: store.ErrBookmarkChanged,
			check: func(t *testing.T, clientset *fake.Clientset) {
				configMap := getConfigMap(t, t.Context(), clientset, testNamespace, testName)
				if got := configMap.Data[dataKey]; got != "500" {
					t.Fatalf("ConfigMap data[%q] got %q, want %q", dataKey, got, "500")
				}
			},
		},
		{
			name: "removes matching bookmark",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
				Data:       map[string]string{dataKey: "100", otherKey: "50"},
			}},
			gvr: key,
			rv:  "100",
			check: func(t *testing.T, clientset *fake.Clientset) {
				configMap := getConfigMap(t, t.Context(), clientset, testNamespace, testName)
				if _, ok := configMap.Data[dataKey]; ok {
					t.Fatalf("ConfigMap data[%q] still present, want removed", dataKey)
				}
				if got := configMap.Data[otherKey]; got != "50" {
					t.Fatalf("ConfigMap data[%q] got %q, want %q", otherKey, got, "50")
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			clientset := fake.NewSimpleClientset(test.objects...)
			store, err := New(clientset, testNamespace, testName)
			if err != nil {
				t.Fatalf("New() got err %v, want nil", err)
			}
			err = store.Delete(ctx, test.gvr, test.rv)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Delete() got err %v, want %v", err, test.wantErr)
			}
			if test.check != nil {
				test.check(t, clientset)
			}
		})
	}
}

func getConfigMap(t *testing.T, ctx context.Context, clientset kubernetes.Interface, namespace, name string) *corev1.ConfigMap {
	t.Helper()

	configMap, err := clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() got err %v, want nil", err)
	}
	return configMap
}
