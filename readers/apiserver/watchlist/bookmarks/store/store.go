package store

import (
	"errors"

	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store/internal/private"
	"github.com/gostdlib/base/context"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ErrBookmarkChanged indicates that a bookmark was not deleted because another writer replaced it.
var ErrBookmarkChanged = errors.New("bookmark changed")

// Bookmarks stores resourceVersions from watch bookmarks so a watcher can resume from a recent bookmark.
type Bookmarks interface {
	// Load returns the stored resourceVersion for gvr. Missing bookmarks return an empty string and a nil error.
	Load(ctx context.Context, gvr schema.GroupVersionResource) (string, error)
	// Store persists resourceVersion for gvr. Empty GVRs or empty resourceVersions return an error.
	Store(ctx context.Context, gvr schema.GroupVersionResource, resourceVersion string) error
	// Delete removes the bookmark only if it matches resourceVersion. It returns ErrBookmarkChanged on a mismatch.
	Delete(ctx context.Context, gvr schema.GroupVersionResource, resourceVersion string) error
	// Package restricts implementations to packages below bookmarks/store.
	Package(private.Package)
}
