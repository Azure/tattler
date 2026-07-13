package fake

import (
	"errors"
	"sync"

	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store"
	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store/internal/private"
	"github.com/gostdlib/base/context"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Store is an in-memory store.Bookmarks implementation for tests that need deterministic bookmark state.
type Store struct {
	mu         sync.Mutex
	values     map[schema.GroupVersionResource]string
	stores     map[schema.GroupVersionResource]string
	storeCalls []map[schema.GroupVersionResource]string
	deletes    []schema.GroupVersionResource
	loadErr    error
	storeErr   error
	deleteErr  error
}

// New returns an in-memory Store seeded with resourceVersion values keyed by Kubernetes resources.
func New(values map[schema.GroupVersionResource]string) *Store {
	copied := map[schema.GroupVersionResource]string{}
	for key, value := range values {
		copied[key] = value
	}
	return &Store{values: copied}
}

func (fakeStore *Store) Load(ctx context.Context, key schema.GroupVersionResource) (string, error) {
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()

	if fakeStore.loadErr != nil {
		return "", fakeStore.loadErr
	}
	return fakeStore.values[key], nil
}

func (fakeStore *Store) Store(ctx context.Context, key schema.GroupVersionResource, resourceVersion string) error {
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()

	fakeStore.storeCalls = append(fakeStore.storeCalls, map[schema.GroupVersionResource]string{key: resourceVersion})
	if fakeStore.storeErr != nil {
		return fakeStore.storeErr
	}
	if fakeStore.stores == nil {
		fakeStore.stores = map[schema.GroupVersionResource]string{}
	}
	if key.Empty() {
		return errors.New("gvr is empty")
	}
	if resourceVersion == "" {
		return errors.New("resourceVersion is empty")
	}
	fakeStore.stores[key] = resourceVersion
	fakeStore.values[key] = resourceVersion
	return nil
}

func (fakeStore *Store) Delete(ctx context.Context, key schema.GroupVersionResource, resourceVersion string) error {
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()

	if key.Empty() {
		return nil
	}
	if resourceVersion == "" {
		return errors.New("resourceVersion is empty")
	}
	if fakeStore.deleteErr != nil {
		return fakeStore.deleteErr
	}
	storedResourceVersion, ok := fakeStore.values[key]
	if !ok {
		return nil
	}
	if storedResourceVersion != resourceVersion {
		return store.ErrBookmarkChanged
	}
	fakeStore.deletes = append(fakeStore.deletes, key)
	delete(fakeStore.values, key)
	return nil
}

func (*Store) Package(private.Package) {}

// SetLoadError configures the error returned by Load for tests covering degraded bookmark storage.
func (fakeStore *Store) SetLoadError(err error) {
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()

	fakeStore.loadErr = err
}

// SetStoreError configures the error returned by Store for tests covering failed bookmark updates.
func (fakeStore *Store) SetStoreError(err error) {
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()

	fakeStore.storeErr = err
}

// SetDeleteError configures the error returned by Delete for tests covering failed bookmark cleanup.
func (fakeStore *Store) SetDeleteError(err error) {
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()

	fakeStore.deleteErr = err
}

// Stored returns the value most recently stored for key so tests can assert bookmark writes.
func (fakeStore *Store) Stored(key schema.GroupVersionResource) string {
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()

	return fakeStore.stores[key]
}

// StoreCalls returns copies of the Store calls.
func (fakeStore *Store) StoreCalls() []map[schema.GroupVersionResource]string {
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()

	calls := make([]map[schema.GroupVersionResource]string, len(fakeStore.storeCalls))
	for i, call := range fakeStore.storeCalls {
		calls[i] = make(map[schema.GroupVersionResource]string, len(call))
		for key, resourceVersion := range call {
			calls[i][key] = resourceVersion
		}
	}
	return calls
}

// Deletes returns the keys deleted from the store so tests can assert stale bookmark cleanup.
func (fakeStore *Store) Deletes() []schema.GroupVersionResource {
	fakeStore.mu.Lock()
	defer fakeStore.mu.Unlock()

	return append([]schema.GroupVersionResource(nil), fakeStore.deletes...)
}
