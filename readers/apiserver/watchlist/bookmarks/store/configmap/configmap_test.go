package configmap

import (
	"testing"

	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/errors"
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
		wantErr   bool
	}{
		{name: "Success: a clientset, namespace and name give a store", clientset: clientset, namespace: testNamespace, cmName: testName},
		{name: "Error: the clientset is nil", clientset: nil, namespace: testNamespace, cmName: testName, wantErr: true},
		{name: "Error: the namespace is empty", clientset: clientset, namespace: "  ", cmName: testName, wantErr: true},
		{name: "Error: the name is empty", clientset: clientset, namespace: testNamespace, cmName: "  ", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store, err := New(test.clientset, test.namespace, test.cmName)
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
			name:    "Error: the ConfigMap must already exist",
			wantErr: apierrors.IsNotFound,
		},
		{
			name: "Success: a missing bookmark loads as empty",
			objects: []runtime.Object{&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: testNamespace},
				Data:       map[string]string{},
			}},
		},
		{
			name: "Success: loads a stored bookmark",
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
			name:    "Error: the ConfigMap must already exist",
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
			name: "Error: the gvr is empty",
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
			name: "Error: the resourceVersion is empty",
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
			name: "Success: initializes the ConfigMap data",
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
			name: "Success: preserves other bookmarks",
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
			name: "Success: retries update conflicts",
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
			name: "Success: ignores a missing ConfigMap",
			gvr:  key,
			rv:   "100",
		},
		{
			name: "Success: ignores an empty gvr",
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
			name: "Success: ignores a missing bookmark",
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
			name: "Error: a replaced bookmark is kept and reported",
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
			name: "Success: removes a matching bookmark",
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

func TestRetryAPIServerErrorClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		err           error
		wantAttempts  int
		wantPermanent bool
	}{
		{name: "Error: not found is permanent", err: apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, testName), wantAttempts: 1, wantPermanent: true},
		{name: "Error: forbidden is permanent", err: apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, testName, errors.New("forbidden")), wantAttempts: 1, wantPermanent: true},
		{name: "Error: unauthorized is permanent", err: apierrors.NewUnauthorized("unauthorized"), wantAttempts: 1, wantPermanent: true},
		{name: "Error: invalid is permanent", err: apierrors.NewInvalid(schema.GroupKind{Kind: "ConfigMap"}, testName, nil), wantAttempts: 1, wantPermanent: true},
		{name: "Error: a conflict is retried", err: apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, testName, errors.New("conflict")), wantAttempts: maxAttempts},
		{name: "Error: a transient error is retried", err: errors.New("temporary API outage"), wantAttempts: maxAttempts},
		{name: "Success: no error makes one attempt", wantAttempts: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			attempts := 0
			err := retryAPIServer(t.Context(), func(context.Context) error {
				attempts++
				return test.err
			})
			if attempts != test.wantAttempts {
				t.Fatalf("retryAPIServer() made %d attempts, want %d", attempts, test.wantAttempts)
			}
			if got := errors.Is(err, exponential.ErrPermanent); got != test.wantPermanent {
				t.Fatalf("retryAPIServer() permanent error got %v, want %v: %v", got, test.wantPermanent, err)
			}
			if !errors.Is(err, test.err) {
				t.Fatalf("retryAPIServer() got err %v, want wrapping %v", err, test.err)
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
