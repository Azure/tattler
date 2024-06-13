// Package items provides the data types needed to implement a filter cache
// for filtering out events that are older than the current version.
package items

import (
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Object represent an object that can be filtered.
type Object interface {
	GetUID() types.UID
	GetResourceVersion() string
	GetGeneration() int64
}

// Item represents the needed fields to filter an object.
// Field aligned for storage reduction.
// Also time is stored as unix nano to reduce storage.
type Item struct {
	// ResourceVersion is the resource version of the object.
	ResourceVersion string
	// LastUpdate is the last time the item was updated in unix nano.
	LastUpdate int64
	// Generation is the generation of the object.
	Generation int64
}

// IsNewer returns true if the item is newer than the cacheable object.
func (i Item) IsNewer(c Object) bool {
	if i.ResourceVersion == c.GetResourceVersion() {
		return false
	}
	if i.Generation >= c.GetGeneration() {
		return true
	}
	return false
}

// New creates a new Item from a Cacheable object.
func New(o Object) Item {
	i := Item{
		ResourceVersion: o.GetResourceVersion(),
		Generation:      o.GetGeneration(),
		LastUpdate:      time.Now().UnixNano(),
	}
	return i
}
