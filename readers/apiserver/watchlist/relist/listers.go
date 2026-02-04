package relist

import (
	"context"
	"fmt"
	"reflect"

	"github.com/Azure/tattler/data"
	"github.com/gostdlib/base/retry/exponential"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var backoff = exponential.Must(exponential.New())

// genericLister is a function that lists resources of type T. It can be any of the common
// client list functions that return a concrete type, such as *corev1.NamespaceList.
// So for example, r.clientset.CoreV1().Namespaces().List is a genericLister[*corev1.NamespaceList].
type genericLister[T runtime.Object] func(ctx context.Context, options metav1.ListOptions) (T, error)

// adapter will turn any genericLister into a ListPage. A generic lister is any of the common client
// list functions that return a concrete type, such as *corev1.NamespaceList. The returned ListPage
// also will handle exponential backoff retries for retriable errors.
func adapter[R runtime.Object](f genericLister[R]) listPage {
	return func(ctx context.Context, options metav1.ListOptions) ([]data.Entry, string, error) {
		obj, err := f(ctx, options)
		if err != nil {
			return nil, "", err
		}
		entries, err := listToEntries(obj)
		if err != nil {
			return nil, "", err
		}
		continueToken, err := getContinue(obj)
		if err != nil {
			return nil, "", err
		}

		return entries, continueToken, err
	}
}

// listToEntries converts a List() result to a slice of data.Entry.
func listToEntries(listResult runtime.Object) ([]data.Entry, error) {
	var events = make([]data.Entry, 0, 100)

	if listResult == nil {
		return nil, fmt.Errorf("listToEntries: listResult is nil: %w", exponential.ErrPermanent)
	}

	// Use reflection to extract Items field from any List type (PodList, NodeList, etc.)
	listValue := reflect.ValueOf(listResult)
	if listValue.Kind() == reflect.Pointer {
		listValue = listValue.Elem()
	}

	itemsField := listValue.FieldByName("Items")
	if !itemsField.IsValid() {
		return nil, fmt.Errorf("listToEntries: no Items field found in %T: %w", listResult, exponential.ErrPermanent)
	}

	for i := 0; i < itemsField.Len(); i++ {
		item := itemsField.Index(i)
		if item.CanInterface() {
			// Get the interface value
			itemInterface := item.Interface()

			// Try as runtime.Object directly first (for pointer types)
			if obj, ok := itemInterface.(runtime.Object); ok {
				events = append(events, data.MustNewEntry(obj, data.STWatchList, data.CTSnapshot))
				continue
			}

			// For value types, we need to take their address
			if !item.CanAddr() {
				return nil, fmt.Errorf("listToEntries: item %d in %T is not addressable: %w", i, listResult, exponential.ErrPermanent)
			}
			itemPtr := item.Addr()
			if !itemPtr.CanInterface() {
				return nil, fmt.Errorf("listToEntries: item %d in %T is not interfaceable: %w", i, listResult, exponential.ErrPermanent)
			}
			if obj, ok := itemPtr.Interface().(runtime.Object); ok {
				events = append(events, data.MustNewEntry(obj, data.STWatchList, data.CTSnapshot))
			}
		}
	}

	return events, nil
}

// getContinue tries to find the Continue token in any List runtime.Object.
// Returns empty string if not found.
func getContinue(obj runtime.Object) (cont string, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("getContinue: %w: %w", err, exponential.ErrPermanent)
		}
	}()

	v := reflect.ValueOf(obj)
	if v.Kind() != reflect.Ptr || v.IsNil() {
		return "", fmt.Errorf("expected non-nil pointer to struct, got %T", obj)
	}

	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return "", fmt.Errorf("expected struct, got %T", obj)
	}

	// Look for ListMeta field
	listMetaField := v.FieldByName("ListMeta")
	if listMetaField.IsValid() && listMetaField.Kind() == reflect.Struct {
		continueField := listMetaField.FieldByName("Continue")
		if continueField.IsValid() && continueField.Kind() == reflect.String {
			return continueField.String(), nil
		}
	}

	// Fallback: look for a direct "Continue" field (just in case)
	continueField := v.FieldByName("Continue")
	if continueField.IsValid() && continueField.Kind() == reflect.String {
		return continueField.String(), nil
	}

	return "", fmt.Errorf("could not find Continue field in %T", obj)
}
