// Package items provides the data types needed to implement a filter cache
// for filtering out events that are older than the current version.
package items

import (
	"k8s.io/apimachinery/pkg/types"
)

// Object represent an object that can be filtered. All K8 objects
// implement this interface.
type Object interface {
	GetUID() types.UID
	GetResourceVersion() string
	GetGeneration() int64
}

// Item represents the needed fields to filter an object.
// Field aligned for storage reduction.
type Item struct {
	// ResourceVersion is the resource version of the object.
	ResourceVersion string
	// Generation is the generation of the object.
	Generation int64
}

//go:generate stringer -type=Age

// Age represents the state of the item compared to the cacheable object.
type Age int8

const (
	// Equal represents the item is equal to the cacheable object.
	Equal Age = 0
	// Older represents the item is older than the cacheable object.
	Older Age = -1
	// Newer represents the item is newer than the cacheable object.
	Newer Age = 1
)

// IsState returns the state of the item compared to the cacheable object.
// If the item is equal to the cacheable object, it returns Equal.
// If the item is newer than the cacheable object, it returns Newer.
// If the item is older than the cacheable object, it returns Older.
func (i Item) IsState(c Object) Age {
	// If the resource version is the same, then the item is equal.
	if i.ResourceVersion == c.GetResourceVersion() {
		return Equal
	}
	switch {
	case i.Generation > c.GetGeneration():
		return Newer
	case i.Generation < c.GetGeneration():
		return Older
	case i.Generation == c.GetGeneration():
		return Equal
	}
	panic("unreachable")
}

// New creates a new Item from a Cacheable object.
func New(o Object) Item {
	i := Item{
		ResourceVersion: o.GetResourceVersion(),
		Generation:      o.GetGeneration(),
	}
	return i
}
