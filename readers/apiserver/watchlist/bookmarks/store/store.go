package store

import (
	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store/internal/private"
	"github.com/gostdlib/base/context"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Bookmarks stores resourceVersions from watch bookmarks so a watcher can resume from a recent bookmark.
type Bookmarks interface {
	// Load returns the stored resourceVersion for gvr. Missing bookmarks return an empty string and a nil error.
	Load(ctx context.Context, gvr schema.GroupVersionResource) (string, error)
	// Store persists resourceVersion for gvr. Empty GVRs or empty resourceVersions may be ignored.
	Store(ctx context.Context, gvr schema.GroupVersionResource, resourceVersion string) error
	// Delete removes any stored resourceVersion for gvr. Missing bookmarks are not errors.
	Delete(ctx context.Context, gvr schema.GroupVersionResource) error
	// Package restricts implementations to packages below bookmarks/store.
	Package(private.Package)
}
