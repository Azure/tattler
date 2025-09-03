package relist

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/Azure/tattler/data"
	"github.com/gostdlib/base/retry/exponential"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// fakeListResult creates a mock list result for testing
type fakeListResult struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []*corev1.Pod `json:"items"`
}

func (m *fakeListResult) DeepCopyObject() runtime.Object {
	panic("not supported")
}

// invalidListResult has no Items field
type invalidListResult struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Data            []*corev1.Pod `json:"data"`
}

func (i *invalidListResult) DeepCopyObject() runtime.Object {
	panic("not supported")
}

// testGenericLister creates a test implementation of genericLister
func testGenericLister(result runtime.Object, err error) genericLister[runtime.Object] {
	return func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
		return result, err
	}
}

func TestAdapter(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name          string
		lister        genericLister[runtime.Object]
		options       metav1.ListOptions
		wantEntries   int
		wantContinue  string
		wantErr       bool
		wantRetriable bool
	}{
		{
			name: "successful list with items",
			lister: testGenericLister(&fakeListResult{
				ListMeta: metav1.ListMeta{Continue: "next-token"},
				Items: []*corev1.Pod{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "pod1",
							UID:  types.UID("uid1"),
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "pod2",
							UID:  types.UID("uid2"),
						},
					},
				},
			}, nil),
			options:      metav1.ListOptions{},
			wantEntries:  2,
			wantContinue: "next-token",
			wantErr:      false,
		},
		{
			name: "empty list",
			lister: testGenericLister(&fakeListResult{
				ListMeta: metav1.ListMeta{Continue: ""},
				Items:    []*corev1.Pod{},
			}, nil),
			options:      metav1.ListOptions{},
			wantEntries:  0,
			wantContinue: "",
			wantErr:      false,
		},
		{
			name:         "lister returns error",
			lister:       testGenericLister(nil, fmt.Errorf("api error: %w", exponential.ErrPermanent)),
			options:      metav1.ListOptions{},
			wantEntries:  0,
			wantContinue: "",
			wantErr:      true,
		},
		{
			name: "invalid list result - no Items field",
			lister: testGenericLister(&invalidListResult{
				ListMeta: metav1.ListMeta{Continue: "token"},
				Data:     []*corev1.Pod{},
			}, nil),
			options:      metav1.ListOptions{},
			wantEntries:  0,
			wantContinue: "",
			wantErr:      true,
		},
	}

	ec := errCheck{testName: "TestAdapter"}
	for _, test := range tests {
		listPageFunc := adapter(test.lister)
		entries, continueToken, err := listPageFunc(ctx, test.options)
		if ec.continueTest(test.name, test.wantErr, err, t) {
			continue
		}

		if len(entries) != test.wantEntries {
			t.Errorf("TestAdapter(%s): adapter() got %d entries, want %d", test.name, len(entries), test.wantEntries)
		}

		if continueToken != test.wantContinue {
			t.Errorf("TestAdapter(%s): adapter() continueToken = %v, want %v", test.name, continueToken, test.wantContinue)
		}

		// Verify entries are properly constructed
		for _, entry := range entries {
			if entry.SourceType() != data.STWatchList {
				t.Errorf("TestAdapter(%s): entry.SourceType() = %v, want %v", test.name, entry.SourceType(), data.STWatchList)
			}
			if entry.ChangeType() != data.CTSnapshot {
				t.Errorf("TestAdapter(%s): entry.ChangeType() = %v, want %v", test.name, entry.ChangeType(), data.CTSnapshot)
			}
		}
	}
}

func TestListToEntries(t *testing.T) {
	tests := []struct {
		name        string
		listResult  runtime.Object
		wantEntries int
		wantErr     bool
	}{
		{
			name: "valid pod list",
			listResult: &fakeListResult{
				Items: []*corev1.Pod{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "pod1",
							UID:  types.UID("uid1"),
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: "pod2",
							UID:  types.UID("uid2"),
						},
					},
				},
			},
			wantEntries: 2,
			wantErr:     false,
		},
		{
			name: "empty pod list",
			listResult: &fakeListResult{
				Items: []*corev1.Pod{},
			},
			wantEntries: 0,
			wantErr:     false,
		},
		{
			name:        "no Items field",
			listResult:  &invalidListResult{},
			wantEntries: 0,
			wantErr:     true,
		},
		{
			name:        "nil input",
			listResult:  nil,
			wantEntries: 0,
			wantErr:     true,
		},
	}

	ec := errCheck{testName: "TestListToEntries"}

	for _, test := range tests {
		entries, err := listToEntries(test.listResult)
		if ec.continueTest(test.name, test.wantErr, err, t) {
			continue
		}

		if len(entries) != test.wantEntries {
			t.Errorf("TestListToEntries(%s): listToEntries() got %d entries, want %d", test.name, len(entries), test.wantEntries)
		}

		// Verify all entries have correct source and change types
		for i, entry := range entries {
			if entry.SourceType() != data.STWatchList {
				t.Errorf("TestListToEntries(%s): entry[%d].SourceType() = %v, want %v", test.name, i, entry.SourceType(), data.STWatchList)
			}
			if entry.ChangeType() != data.CTSnapshot {
				t.Errorf("TestListToEntries(%s): entry[%d].ChangeType() = %v, want %v", test.name, i, entry.ChangeType(), data.CTSnapshot)
			}
		}

		// Verify permanent error wrapping for errors
		if err != nil && !errors.Is(err, exponential.ErrPermanent) {
			t.Errorf("TestListToEntries(%s): listToEntries() error should be wrapped with exponential.ErrPermanent", test.name)
		}
	}
}

func TestGetContinue(t *testing.T) {
	tests := []struct {
		name         string
		obj          runtime.Object
		wantContinue string
		wantErr      bool
	}{
		{
			name: "valid object with continue token",
			obj: &fakeListResult{
				ListMeta: metav1.ListMeta{
					Continue: "next-page-token",
				},
			},
			wantContinue: "next-page-token",
			wantErr:      false,
		},
		{
			name: "valid object with empty continue token",
			obj: &fakeListResult{
				ListMeta: metav1.ListMeta{
					Continue: "",
				},
			},
			wantContinue: "",
			wantErr:      false,
		},
		{
			name:         "nil object",
			obj:          nil,
			wantContinue: "",
			wantErr:      true,
		},
		{
			name: "object without ListMeta field",
			obj: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
				},
			},
			wantContinue: "",
			wantErr:      true,
		},
	}

	ec := errCheck{testName: "TestGetContinue"}

	for _, test := range tests {
		continueToken, err := getContinue(test.obj)
		if ec.continueTest(test.name, test.wantErr, err, t) {
			continue
		}

		if continueToken != test.wantContinue {
			t.Errorf("TestGetContinue(%s): got %v, want %v", continueToken, test.wantContinue, test.name)
			continue
		}

		// Verify permanent error wrapping for errors
		if err != nil && !errors.Is(err, exponential.ErrPermanent) {
			t.Errorf("TestGetContinue(%s): getContinue() error should be wrapped with exponential.ErrPermanent", test.name)
		}
	}
}

func TestListToEntriesReflection(t *testing.T) {
	// Test that reflection works correctly with different pointer vs value types
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
			UID:  types.UID("test-uid"),
		},
	}

	// Test with pointer to struct
	ptrResult := &fakeListResult{
		Items: []*corev1.Pod{pod},
	}

	entries, err := listToEntries(ptrResult)
	if err != nil {
		t.Errorf("TestListToEntriesReflection: listToEntries() with pointer failed: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("TestListToEntriesReflection: listToEntries() with pointer got %d entries, want 1", len(entries))
	}

	// Test reflection edge cases
	listValue := reflect.ValueOf(ptrResult)
	if listValue.Kind() != reflect.Ptr {
		t.Error("TestListToEntriesReflection: Expected pointer type")
	}

	listValue = listValue.Elem()
	if listValue.Kind() != reflect.Struct {
		t.Error("TestListToEntriesReflection: Expected struct type after dereferencing pointer")
	}

	itemsField := listValue.FieldByName("Items")
	if !itemsField.IsValid() {
		t.Error("TestListToEntriesReflection: Items field should be valid")
	}

	if itemsField.Len() != 1 {
		t.Errorf("TestListToEntriesReflection: Items field length = %d, want 1", itemsField.Len())
	}
}

func TestAdapterRetryBehavior(t *testing.T) {
	ctx := context.Background()
	callCount := 0

	// Create a lister that fails on first call, succeeds on second
	retryLister := func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
		callCount++
		if callCount == 1 {
			// Return a retriable error (no exponential.ErrPermanent wrapping)
			return nil, errors.New("TestAdapterRetryBehavior: temporary failure")
		}
		return &fakeListResult{
			Items: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod1",
						UID:  types.UID("uid1"),
					},
				},
			},
		}, nil
	}

	// Test the genericLister directly with adapter
	adaptedLister := adapter(retryLister)
	entries, _, err := adaptedLister(ctx, metav1.ListOptions{})

	if err != nil {
		t.Errorf("TestAdapterRetryBehavior: adapter() should eventually succeed with retry, got error: %v", err)
	}

	if len(entries) != 1 {
		t.Errorf("TestAdapterRetryBehavior: adapter() got %d entries, want 1", len(entries))
	}

	if callCount < 2 {
		t.Errorf("TestAdapterRetryBehavior: Expected at least 2 calls due to retry, got %d", callCount)
	}
}

// Benchmark tests
func BenchmarkListToEntries(b *testing.B) {
	// Create a large list for benchmarking
	items := make([]*corev1.Pod, 1000)
	for i := range items {
		items[i] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-" + string(rune(i)),
				UID:  types.UID("uid-" + string(rune(i))),
			},
		}
	}

	listResult := &fakeListResult{Items: items}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := listToEntries(listResult)
		if err != nil {
			b.Fatalf("listToEntries() failed: %v", err)
		}
	}
}

func BenchmarkGetContinue(b *testing.B) {
	obj := &fakeListResult{
		ListMeta: metav1.ListMeta{
			Continue: "some-long-continue-token-for-pagination",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := getContinue(obj)
		if err != nil {
			b.Fatalf("getContinue() failed: %v", err)
		}
	}
}

type errCheck struct {
	testName string
}

func (e errCheck) continueTest(tableName string, wantErr bool, err error, t *testing.T) (continueTest bool) {
	switch {
	case err == nil && wantErr:
		t.Errorf("%s(%s): got err == nil, want err != nil", e.testName, tableName)
		return true
	case err != nil && !wantErr:
		t.Errorf("%s(%s): got err != nil, want err == nil: %v", e.testName, tableName, err)
		return true
	case err != nil && wantErr:
		// Expected error case - test passed, continue to next test
		return true
	}
	return false
}
