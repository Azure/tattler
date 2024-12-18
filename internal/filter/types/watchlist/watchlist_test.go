package watchlist

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/internal/filter/items"
	"github.com/kylelemons/godebug/pretty"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

type fakeObject struct {
	uuid            types.UID
	resourceVersion string
	generation      int64
}

func (f fakeObject) GetUID() types.UID {
	return f.uuid
}

func (f fakeObject) GetResourceVersion() string {
	return f.resourceVersion
}

func (f fakeObject) GetGeneration() int64 {
	return f.generation
}

func (f fakeObject) DeepCopyObject() runtime.Object {
	return f
}

func (f fakeObject) GetObjectKind() schema.ObjectKind {
	return nil
}

func TestHandleEvent(t *testing.T) {
	tests := []struct {
		name               string
		event              watch.Event
		expectEventForward bool
	}{
		{
			name: "Older Object Not Forwarded",
			event: watch.Event{
				Type: watch.Modified,
				Object: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						UID:             types.UID("1"),
						ResourceVersion: "rabbit",
						Generation:      0,
					},
				},
			},
		},
		{
			name: "Cached Object forwarded",
			event: watch.Event{
				Type: watch.Modified,
				Object: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						UID:             types.UID("1"),
						ResourceVersion: "elephant",
						Generation:      2,
					},
				},
			},
			expectEventForward: true,
		},
		{
			name: "Snapshot Object forwarded",
			event: watch.Event{
				Type: watch.Added,
				Object: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						UID:             types.UID("1"),
						ResourceVersion: "pony",
						Generation:      1,
					},
				},
			},
			expectEventForward: true,
		},
	}

	for _, test := range tests {
		f := &Filter{
			logger: slog.Default(),
			m: map[types.UID]items.Item{
				"1": {
					ResourceVersion: "pony",
					Generation:      1,
				},
			},
			out: make(chan data.Entry, 1),
		}

		f.handleEvent(context.Background(), test.event)

		if test.expectEventForward {
			select {
			case <-f.out:
			default:
				t.Errorf("TestHandleEvent(%s): expected event to be forwarded", test.name)
			}
		} else {
			select {
			case <-f.out:
				t.Errorf("TestHandleEvent(%s): expected event not to be forwarded", test.name)
			default:
			}
		}
	}
}

func TestEventToEntry(t *testing.T) {
	t.Parallel()

	// Set managedFields for Update and creationTimestamp for Add for deterministic changeTime in results.
	managedFields := []metav1.ManagedFieldsEntry{
		{
			Manager:    "test",
			Operation:  "Apply",
			APIVersion: "v1",
			Time:       &metav1.Time{Time: time.Now()},
		},
	}
	creationTimestamp := metav1.Time{Time: time.Now().Add(-time.Hour)}

	tests := []struct {
		name       string
		ctx        context.Context
		event      watch.Event
		isSnapshot bool
		want       data.Entry
	}{
		{
			name: "Added event",
			event: watch.Event{
				Type: watch.Added,
				Object: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						CreationTimestamp: creationTimestamp,
					},
				},
			},
			want: data.MustNewEntry(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: creationTimestamp}}, data.STWatchList, data.CTAdd),
		},
		{
			name: "Modified event",
			event: watch.Event{
				Type: watch.Modified,
				Object: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						CreationTimestamp: creationTimestamp,
						ManagedFields:     managedFields,
					},
				},
			},
			want: data.MustNewEntry(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: creationTimestamp, ManagedFields: managedFields}}, data.STWatchList, data.CTUpdate),
		},
		{
			name: "Deleted event",
			event: watch.Event{
				Type: watch.Deleted,
				Object: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						CreationTimestamp: creationTimestamp,
						ManagedFields:     managedFields,
					},
				},
			},
			want: data.MustNewEntry(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: creationTimestamp, ManagedFields: managedFields}}, data.STWatchList, data.CTDelete),
		},
		{
			name: "Snapshot event",
			event: watch.Event{
				Type: watch.Added,
				Object: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						CreationTimestamp: creationTimestamp,
						ManagedFields:     managedFields,
					},
				},
			},
			isSnapshot: true,
			want:       data.MustNewEntry(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: creationTimestamp, ManagedFields: managedFields}}, data.STWatchList, data.CTSnapshot),
		},
	}

	for _, test := range tests {
		f := &Filter{}

		got, err := f.eventToEntry(test.event, test.isSnapshot)
		if err != nil {
			panic(err)
		}

		if diff := pretty.Compare(test.want, got); diff != "" {
			t.Errorf("TestEventToEntry(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}

func TestSetMapItem(t *testing.T) {
	t.Parallel()

	// Note: resourceVersion is only used for equality checks. If it is the same,
	// the objects are considered the same. It can be any value, though it is a UUID in practice.
	// If resourceVersion is not the same, you use generation to determine if the object is newer.

	tests := []struct {
		name           string
		event          watch.Event
		wantCached     bool
		wantIsSnapshot bool
		wantIsDeleted  bool
	}{
		{
			name: "Delete event",
			event: watch.Event{
				Type: watch.Deleted,
				Object: fakeObject{
					uuid:            "1",
					resourceVersion: "1",
					generation:      0,
				},
			},
			wantCached:     false,
			wantIsSnapshot: false,
			wantIsDeleted:  true,
		},
		{
			name: "Item not in cache",
			event: watch.Event{
				Type: watch.Added,
				Object: fakeObject{
					uuid:            "2",
					resourceVersion: "1",
					generation:      0,
				},
			},
			wantCached:     true,
			wantIsSnapshot: false,
			wantIsDeleted:  false,
		},
		{
			name: "Object is older than the cached item",
			event: watch.Event{
				Type: watch.Modified,
				Object: fakeObject{
					uuid:            "3",
					resourceVersion: "rabbit", // This only matters in equality to tell if objects are the same
					generation:      1,        // This is what indicates it is older
				},
			},
			wantCached:     false,
			wantIsSnapshot: false,
			wantIsDeleted:  false,
		},
		{
			name: "Object is equal to the cached item",
			event: watch.Event{
				Type: watch.Modified,
				Object: fakeObject{
					uuid:            "3",
					resourceVersion: "3",
					generation:      2,
				},
			},
			wantIsSnapshot: true,
			wantIsDeleted:  false,
		},
		{
			name: "Object is newer than the cached item",
			event: watch.Event{
				Type: watch.Modified,
				Object: fakeObject{
					uuid:            "3",
					resourceVersion: "apples",
					generation:      3,
				},
			},
			wantCached:     true,
			wantIsSnapshot: false,
			wantIsDeleted:  false,
		},
	}

	for _, test := range tests {
		f := &Filter{
			m: map[types.UID]items.Item{
				"1": {
					ResourceVersion: "1",
					Generation:      0,
				},
				"3": {
					ResourceVersion: "3",
					Generation:      2,
				},
			},
		}

		obj := test.event.Object.(items.Object)

		gotCached, gotIsSnapshot, gotIsDeleted := f.setMapItem(obj, test.event)
		if gotCached != test.wantCached {
			t.Errorf("TestSetMapItem(%s): got cached %t, want %t", test.name, gotCached, test.wantCached)
		}
		if gotIsSnapshot != test.wantIsSnapshot {
			t.Errorf("TestSetMapItem(%s): got isSnapshot %t, want %t", test.name, gotIsSnapshot, test.wantIsSnapshot)
		}
		if gotIsDeleted != test.wantIsDeleted {
			t.Errorf("TestSetMapItem(%s): got isDeleted %t, want %t", test.name, gotIsDeleted, test.wantIsDeleted)
		}

		if test.wantIsDeleted {
			if _, ok := f.m["1"]; ok {
				t.Errorf("TestSetMapItem(%s): item not deleted", test.name)
			}
		}
	}
}
