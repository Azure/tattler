package relist

import (
	"fmt"
	"testing"

	"github.com/Azure/tattler/data"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/errors"
	"github.com/gostdlib/base/retry/exponential"
	"github.com/kylelemons/godebug/pretty"
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
	t.Parallel()

	ctx := t.Context()

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
			name: "Success: a list with items gives an entry per item and the continue token",
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
			name: "Success: an empty list gives no entries",
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
			name:         "Error: the lister fails",
			lister:       testGenericLister(nil, fmt.Errorf("api error: %w", exponential.ErrPermanent)),
			options:      metav1.ListOptions{},
			wantEntries:  0,
			wantContinue: "",
			wantErr:      true,
		},
		{
			name: "Error: the list result has no Items field",
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
	t.Parallel()

	tests := []struct {
		name        string
		listResult  runtime.Object
		wantEntries int
		// wantUIDs, when set, are the UIDs of the entries in order.
		wantUIDs []types.UID
		wantErr  bool
	}{
		{
			name: "Success: a typed list holding value items gives each entry its own item",
			listResult: &corev1.PodList{
				Items: []corev1.Pod{
					{ObjectMeta: metav1.ObjectMeta{Name: "pod1", UID: types.UID("uid1")}},
					{ObjectMeta: metav1.ObjectMeta{Name: "pod2", UID: types.UID("uid2")}},
				},
			},
			wantEntries: 2,
			wantUIDs:    []types.UID{"uid1", "uid2"},
		},
		{
			name: "Success: a list holding pointer items gives each entry its own item",
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
			wantUIDs:    []types.UID{"uid1", "uid2"},
		},
		{
			name: "Success: an empty list gives no entries",
			listResult: &fakeListResult{
				Items: []*corev1.Pod{},
			},
			wantEntries: 0,
			wantErr:     false,
		},
		{
			name:        "Error: the list result has no Items field",
			listResult:  &invalidListResult{},
			wantEntries: 0,
			wantErr:     true,
		},
		{
			name:        "Error: the list result is nil",
			listResult:  nil,
			wantEntries: 0,
			wantErr:     true,
		},
	}

	ec := errCheck{testName: "TestListToEntries"}

	for _, test := range tests {
		entries, err := listToEntries(test.listResult)
		// retryableList stops retrying on exponential.ErrPermanent, and a malformed list result never gets better.
		if err != nil && !errors.Is(err, exponential.ErrPermanent) {
			t.Errorf("TestListToEntries(%s): listToEntries() error should be wrapped with exponential.ErrPermanent", test.name)
		}
		if ec.continueTest(test.name, test.wantErr, err, t) {
			continue
		}

		if len(entries) != test.wantEntries {
			t.Errorf("TestListToEntries(%s): listToEntries() got %d entries, want %d", test.name, len(entries), test.wantEntries)
		}

		if test.wantUIDs != nil {
			var gotUIDs []types.UID
			for _, entry := range entries {
				gotUIDs = append(gotUIDs, entry.UID())
			}
			if diff := pretty.Compare(test.wantUIDs, gotUIDs); diff != "" {
				t.Errorf("TestListToEntries(%s): entry UIDs -want/+got:\n%s", test.name, diff)
			}
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
	}
}

func TestGetContinue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		obj          runtime.Object
		wantContinue string
		wantErr      bool
	}{
		{
			name: "Success: returns the continue token",
			obj: &fakeListResult{
				ListMeta: metav1.ListMeta{
					Continue: "next-page-token",
				},
			},
			wantContinue: "next-page-token",
			wantErr:      false,
		},
		{
			name: "Success: an empty continue token means the last page",
			obj: &fakeListResult{
				ListMeta: metav1.ListMeta{
					Continue: "",
				},
			},
			wantContinue: "",
			wantErr:      false,
		},
		{
			name:         "Error: the object is nil",
			obj:          nil,
			wantContinue: "",
			wantErr:      true,
		},
		{
			name: "Error: the object has no ListMeta field",
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
		// retryableList stops retrying on exponential.ErrPermanent, and a malformed list result never gets better.
		if err != nil && !errors.Is(err, exponential.ErrPermanent) {
			t.Errorf("TestGetContinue(%s): getContinue() error should be wrapped with exponential.ErrPermanent", test.name)
		}
		if ec.continueTest(test.name, test.wantErr, err, t) {
			continue
		}

		if continueToken != test.wantContinue {
			t.Errorf("TestGetContinue(%s): got %v, want %v", test.name, continueToken, test.wantContinue)
		}
	}
}

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

	// Typed Kubernetes lists, such as PodList, hold their items as values, which is the path relists take.
	podList := &corev1.PodList{Items: make([]corev1.Pod, len(items))}
	for i, pod := range items {
		podList.Items[i] = *pod
	}

	benchmarks := []struct {
		name string
		list runtime.Object
	}{
		{name: "pointer items", list: &fakeListResult{Items: items}},
		{name: "value items", list: podList},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := listToEntries(bm.list); err != nil {
					b.Fatalf("listToEntries() failed: %v", err)
				}
			}
		})
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
