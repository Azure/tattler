// Package data provides data types for readers. All data types for readers are
// packages inside an Entry. This allows for a single channel to be used for all
// data types.
package data

import (
	"errors"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

var (
	// ErrInvalidType is returned when the type is invalid.
	ErrInvalidType = errors.New("invalid type")
)

//go:generate stringer -type=SourceType -linecomment

type SourceType uint8

const (
	//
	STUnknown   SourceType = 0 // Unknown
	STInformer  SourceType = 1 // Informer
	STWatchList SourceType = 2 // WatchList
)

//go:generate stringer -type=ObjectType -linecomment

// ObjectType is the type of the object held in a type.
type ObjectType uint8

const (
	// OTUnknown indicates a bug in the code.
	OTUnknown ObjectType = 0 // Unknown
	// OTNode indicates the data is a node.
	OTNode ObjectType = 1 // Node
	// OTPod indicates the data is a pod.
	OTPod ObjectType = 2 // Pod
	// OTNamespace indicates the data is a namespace.
	OTNamespace ObjectType = 3 // Namespace
	// OTPersistentVolume indicates the data is a persistent volume.
	OTPersistentVolume ObjectType = 4 // PersistentVolume
)

// ChangeType is the type of change.
type ChangeType uint8

const (
	// CTUnknown indicates a bug in the code.
	CTUnknown ChangeType = 0
	// CTAdd indicates the data was added.
	CTAdd ChangeType = 1
	// CTUpdate indicates the data was updated.
	CTUpdate ChangeType = 2
	// CTDelete indicates the data was deleted.
	CTDelete ChangeType = 3
)

// ingestObj is a generic type for objects that can be ingested.
type ingestObj interface {
	*corev1.Node | *corev1.Pod | *corev1.Namespace | *corev1.PersistentVolume

	runtime.Object

	GetUID() types.UID
}

// Entry is a data entry.
// This is field aligned for better performance.
type Entry struct {
	// data holds the data.
	data runtime.Object
	// uid is the UID of the object.
	uid types.UID

	// SourceType is the type of the entry.
	sourceType SourceType
	// ChangeType is the type of change.
	changeType ChangeType
	// ObjectType is the type of the object.
	objectType ObjectType
}

// NewEntry creates a new Entry. The underlying object must be a corev1.Node, corev1.Pod,
// corev1.PersistentVolume or corev1.Namespace.
func NewEntry(obj runtime.Object, st SourceType, ct ChangeType) (Entry, error) {
	switch v := any(obj).(type) {
	case *corev1.Node:
		return newEntry(v, st, ct)
	case *corev1.Pod:
		return newEntry(v, st, ct)
	case *corev1.Namespace:
		return newEntry(v, st, ct)
	case *corev1.PersistentVolume:
		return newEntry(v, st, ct)
	}
	return Entry{}, ErrInvalidType
}

// newEntry creates a new Entry.
func newEntry[O ingestObj](obj O, st SourceType, ct ChangeType) (Entry, error) {
	nv := reflect.ValueOf(obj)
	if nv.IsValid() {
		if nv.IsZero() {
			return Entry{}, fmt.Errorf("new object is nil")
		}
	}

	var ot ObjectType
	switch v := any(obj).(type) {
	case *corev1.Node:
		ot = OTNode
	case *corev1.Pod:
		ot = OTPod
	case *corev1.Namespace:
		ot = OTNamespace
	case *corev1.PersistentVolume:
		ot = OTPersistentVolume
	default:
		return Entry{}, fmt.Errorf("unknown object type(%T)", v)
	}

	return Entry{
		data:       obj,
		uid:        obj.GetUID(),
		sourceType: st,
		changeType: ct,
		objectType: ot,
	}, nil
}

// MustNewEntry creates a new Entry. It panics if an error occurs.
func MustNewEntry[T runtime.Object](obj T, st SourceType, ct ChangeType) Entry {
	e, err := NewEntry(obj, st, ct)
	if err != nil {
		panic(err)
	}
	return e
}

// UID returns the UID of the underlying object. This is always the latest change.
func (e Entry) UID() types.UID {
	return e.uid
}

// ObjectType returns the type of the object.
func (e Entry) ObjectType() ObjectType {
	return e.objectType
}

// ChangeType returns the type of change.
func (e Entry) ChangeType() ChangeType {
	return e.changeType
}

// SourceType returns the source type.
func (e Entry) SourceType() SourceType {
	return e.sourceType
}

// Object returns the data as a runtime.Object.
func (e Entry) Object() runtime.Object {
	return e.data
}

// Node returns the data for a Node type change. An error is returned if the type is not Node.
func (e Entry) Node() (*corev1.Node, error) {
	return assert[*corev1.Node](e.data)
}

// Pod returns the data a pod type change. An error is returned if the type is not Pod.
func (e Entry) Pod() (*corev1.Pod, error) {
	return assert[*corev1.Pod](e.data)
}

// Namespace returns the data as a namespace type change. An error is returned if the type is not Namespace.
func (e Entry) Namespace() (*corev1.Namespace, error) {
	return assert[*corev1.Namespace](e.data)
}

// PersistentVolume returns the data as a persistent volume type change. An error is returned if the type is not PersistentVolume.
func (e Entry) PersistentVolume() (*corev1.PersistentVolume, error) {
	return assert[*corev1.PersistentVolume](e.data)
}

// assert asserts the type of the object to the type specfied by the AssertTo generic type.
func assert[AssertTo runtime.Object](obj runtime.Object) (AssertTo, error) {
	var empty AssertTo

	if obj == nil {
		return empty, fmt.Errorf("object is nil")
	}

	v, ok := obj.(AssertTo)
	if !ok {
		return empty, fmt.Errorf("object is not of type %T", empty)
	}

	return v, nil
}
