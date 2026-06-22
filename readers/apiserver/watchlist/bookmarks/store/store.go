package store

import (
	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store/internal/private"
	"github.com/gostdlib/base/context"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Bookmarks stores resourceVersions from watch bookmarks so a watcher can resume from a recent bookmark.
type Bookmarks interface {
	Load(ctx context.Context, gvr schema.GroupVersionResource) (string, error)
	Store(ctx context.Context, gvr schema.GroupVersionResource, resourceVersion string) error
	Delete(ctx context.Context, gvr schema.GroupVersionResource) error
	PackagePrivate(private.Token)
}
