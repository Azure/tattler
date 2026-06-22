package fake

import (
	"sync"

	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store/internal/private"
	"github.com/gostdlib/base/context"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Store is an in-memory store.Bookmarks implementation for tests that need deterministic bookmark state.
type Store struct {
	mu       sync.Mutex
	values   map[schema.GroupVersionResource]string
	stores   map[schema.GroupVersionResource]string
	deletes  []schema.GroupVersionResource
	loadErr  error
	storeErr error
}

// New returns an in-memory Store seeded with resourceVersion values keyed by Kubernetes resources.
func New(values map[schema.GroupVersionResource]string) *Store {
	copied := map[schema.GroupVersionResource]string{}
	for key, value := range values {
		copied[key] = value
	}
	return &Store{values: copied}
}

func (store *Store) Load(ctx context.Context, key schema.GroupVersionResource) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	if store.loadErr != nil {
		return "", store.loadErr
	}
	return store.values[key], nil
}

func (store *Store) Store(ctx context.Context, key schema.GroupVersionResource, resourceVersion string) error {
	store.mu.Lock()
	defer store.mu.Unlock()

	if store.storeErr != nil {
		return store.storeErr
	}
	if store.stores == nil {
		store.stores = map[schema.GroupVersionResource]string{}
	}
	store.stores[key] = resourceVersion
	store.values[key] = resourceVersion
	return nil
}

func (store *Store) Delete(ctx context.Context, key schema.GroupVersionResource) error {
	store.mu.Lock()
	defer store.mu.Unlock()

	store.deletes = append(store.deletes, key)
	delete(store.values, key)
	return nil
}

func (*Store) PackagePrivate(private.Token) {}

// SetLoadError configures the error returned by Load for tests covering degraded bookmark storage.
func (store *Store) SetLoadError(err error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	store.loadErr = err
}

// SetStoreError configures the error returned by Store for tests covering failed bookmark updates.
func (store *Store) SetStoreError(err error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	store.storeErr = err
}

// Stored returns the value most recently stored for key so tests can assert bookmark writes.
func (store *Store) Stored(key schema.GroupVersionResource) string {
	store.mu.Lock()
	defer store.mu.Unlock()

	return store.stores[key]
}

// Deletes returns the keys deleted from the store so tests can assert stale bookmark cleanup.
func (store *Store) Deletes() []schema.GroupVersionResource {
	store.mu.Lock()
	defer store.mu.Unlock()

	return append([]schema.GroupVersionResource(nil), store.deletes...)
}
