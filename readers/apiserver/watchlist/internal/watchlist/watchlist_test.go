package watchlist

import (
	"context"
	"errors"
	"log"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/kylelemons/godebug/pretty"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

// To allow embedding in the fakeObject struct, because they
// have the same Object name.
type rto = runtime.Object
type mto = metav1.Object

type fakeObject struct {
	rto
	mto

	rv string
}

func (f *fakeObject) GetResourceVersion() string {
	return f.rv
}

type fakeWatcher struct {
	watch.Interface
}

func TestRun(t *testing.T) {
	t.Parallel()

	watchesCalled := []RetrieveType{}
	var packageEventsCalled chan struct{}

	tests := []struct {
		name              string
		started           bool
		ch                chan data.Entry
		retrieveTypes     RetrieveType
		cancelWatcher     bool
		fakeWatch         func(context.Context, RetrieveType, spanWatcher) error
		wantPackageEvents bool
		wantRetrieveTypes []RetrieveType
		wantErr           bool
	}{
		{
			name:    "Error: already started",
			started: true,
			wantErr: true,
		},
		{
			name:    "Error: .ch is nil",
			ch:      nil,
			wantErr: true,
		},
		{
			name:          "Error: Namespace watch returns error",
			ch:            make(chan data.Entry, 1),
			retrieveTypes: RTNamespace,
			fakeWatch: func(ctx context.Context, rt RetrieveType, spanWatcher spanWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return errors.New("error")
			},
			wantRetrieveTypes: []RetrieveType{RTNamespace},
			wantPackageEvents: true,
			wantErr:           true,
		},
		{
			name:          "Error: PersistentVolume watch returns error",
			ch:            make(chan data.Entry, 1),
			retrieveTypes: RTPersistentVolume,
			fakeWatch: func(ctx context.Context, rt RetrieveType, spanWatcher spanWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return errors.New("error")
			},
			wantRetrieveTypes: []RetrieveType{RTPersistentVolume},
			wantPackageEvents: true,
			wantErr:           true,
		},
		{
			name:          "Error: Node watch returns error",
			ch:            make(chan data.Entry, 1),
			retrieveTypes: RTNode,
			fakeWatch: func(ctx context.Context, rt RetrieveType, spanWatcher spanWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return errors.New("error")
			},
			wantRetrieveTypes: []RetrieveType{RTNode},
			wantPackageEvents: true,
			wantErr:           true,
		},
		{
			name:          "Namespace success",
			ch:            make(chan data.Entry, 1),
			retrieveTypes: RTNamespace,
			fakeWatch: func(ctx context.Context, rt RetrieveType, spanWatcher spanWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				log.Println(watchesCalled)
				return nil
			},
			wantRetrieveTypes: []RetrieveType{RTNamespace},
			wantPackageEvents: true,
		},
		{
			name:          "PersistentVolume success",
			ch:            make(chan data.Entry, 1),
			retrieveTypes: RTPersistentVolume,
			fakeWatch: func(ctx context.Context, rt RetrieveType, spanWatcher spanWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieveTypes: []RetrieveType{RTPersistentVolume},
			wantPackageEvents: true,
		},
		{
			name:          "Node success",
			ch:            make(chan data.Entry, 1),
			retrieveTypes: RTNode,
			fakeWatch: func(ctx context.Context, rt RetrieveType, spanWatcher spanWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieveTypes: []RetrieveType{RTNode},
			wantPackageEvents: true,
		},
		{
			name:          "Pod success",
			ch:            make(chan data.Entry, 1),
			retrieveTypes: RTPod,
			fakeWatch: func(ctx context.Context, rt RetrieveType, spanWatcher spanWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieveTypes: []RetrieveType{RTPod},
			wantPackageEvents: true,
		},
		{
			name:          "All success",
			ch:            make(chan data.Entry, 1),
			retrieveTypes: RTNamespace | RTPersistentVolume | RTNode | RTPod,
			fakeWatch: func(ctx context.Context, rt RetrieveType, spanWatcher spanWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieveTypes: []RetrieveType{RTNamespace, RTPersistentVolume, RTNode, RTPod},
			wantPackageEvents: true,
		},
	}

	for _, test := range tests {
		watchesCalled = nil
		packageEventsCalled = make(chan struct{})

		r := &Reader{
			started:       test.started,
			ch:            test.ch,
			retrieveTypes: test.retrieveTypes,
			fakeWatch:     test.fakeWatch,
			fakePackageEvents: func(ctx context.Context) {
				close(packageEventsCalled)
			},
		}

		err := r.Run(context.Background())
		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestRun(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("TestRun(%s): got err == %v, want err == nil", test.name, err)
			continue
		}

		if test.wantPackageEvents {
			select {
			case <-packageEventsCalled:
			case <-time.After(1 * time.Second):
				t.Errorf("TestRun(%s): packageEvents was not called", test.name)
			}
		}

		slices.Sort[[]RetrieveType, RetrieveType](watchesCalled)
		slices.Sort[[]RetrieveType, RetrieveType](test.wantRetrieveTypes)
		if diff := pretty.Compare(test.wantRetrieveTypes, watchesCalled); diff != "" {
			t.Errorf("TestRun(%s): retrieveTypes: -want/+got:\n%s", test.name, diff)
		}
	}

}

func TestSetupCache(t *testing.T) {
	t.Parallel()

	r := &Reader{}
	if err := r.setupFilter(context.Background()); err != nil {
		t.Fatalf("TestSetupCache: got err == %v, want err == nil", err)
	}
	defer close(r.filterIn)

	if r.filter == nil {
		t.Errorf("TestSetupCache: got cache == nil, want cache != nil")
	}
	if r.filterIn == nil {
		t.Errorf("TestSetupCache: got cacheIn == nil, want cacheIn != nil")
	}
	if r.filterOut == nil {
		t.Errorf("TestSetupCache: got cacheOut == nil, want cacheOut != nil")
	}
}

func TestPackageEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		ctx   context.Context
		event watch.Event
		want  data.Entry
	}{
		{
			name: "Added event",
			event: watch.Event{
				Type:   watch.Added,
				Object: &corev1.Pod{},
			},
			want: data.MustNewEntry(&corev1.Pod{}, data.STWatchList, data.CTAdd),
		},
		{
			name: "Modified event",
			event: watch.Event{
				Type:   watch.Modified,
				Object: &corev1.Pod{},
			},
			want: data.MustNewEntry(&corev1.Pod{}, data.STWatchList, data.CTUpdate),
		},
		{
			name: "Deleted event",
			event: watch.Event{
				Type:   watch.Deleted,
				Object: &corev1.Pod{},
			},
			want: data.MustNewEntry(&corev1.Pod{}, data.STWatchList, data.CTDelete),
		},
	}

	for _, test := range tests {
		r := &Reader{
			filterOut: make(chan watch.Event, 1),
			ch:        make(chan data.Entry, 1),
		}
		r.filterOut <- test.event
		close(r.filterOut)
		r.packageEvents(context.Background())

		got := <-r.ch
		if diff := pretty.Compare(test.want, got); diff != "" {
			t.Errorf("TestPackageEvents(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}

func TestWatch(t *testing.T) {
	t.Parallel()

	doneCtx, cancel := context.WithCancel(context.Background())
	cancel()

	eventWatcherCount := 0

	tests := []struct {
		name         string
		ctx          context.Context
		spanWatcher  func(metav1.ListOptions) (watch.Interface, error)
		eventWatcher func(ctx context.Context, watcher watch.Interface) (string, error)
		wantErr      bool
	}{
		{
			name: "Context done",
			ctx:  doneCtx,
		},
		{
			name: "Watching had connection error and we haven't connected before",
			ctx:  context.Background(),
			spanWatcher: func(options metav1.ListOptions) (watch.Interface, error) {
				return nil, errors.New("error")
			},
			wantErr: true,
		},
		{
			name: "Watching had connection error but we have connected before",
			ctx:  context.Background(),
			spanWatcher: func(options metav1.ListOptions) (watch.Interface, error) {
				return struct{ watch.Interface }{}, nil
			},
			eventWatcher: func(ctx context.Context, watcher watch.Interface) (string, error) {
				if eventWatcherCount == 0 {
					eventWatcherCount++
					return "", errors.New("error")
				}
				return "", errors.New("error")
			},
		},
	}

	for _, test := range tests {
		log.Printf("TestWatch(%s):", test.name)
		eventWatcherCount = 0

		r := &Reader{
			fakeWatchEvents: test.eventWatcher,
		}

		var ctx context.Context
		if test.ctx.Err() == nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(context.Background())
			go func() {
				time.Sleep(100 * time.Millisecond)
				cancel()
			}()
		} else {
			ctx = test.ctx
		}

		err := r.watch(ctx, RTNamespace, test.spanWatcher)
		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestWatch(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("TestWatch(%s): got err == %v, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}
	}
}

func TestWatchEvent(t *testing.T) {
	t.Parallel()

	newStopper := func(stopped *bool) func() {
		return func() {
			*stopped = true
		}
	}

	doneCtx, cancel := context.WithCancel(context.Background())
	cancel()

	closedIn := make(chan watch.Event)
	close(closedIn)

	tests := []struct {
		name        string
		ctx         context.Context
		ch          chan watch.Event
		wantRV      string
		event       watch.Event
		wantEvent   watch.Event
		wantErr     bool
		wantStopper bool
	}{
		{
			name:        "Context done",
			ctx:         doneCtx,
			wantErr:     true,
			wantStopper: true,
		},
		{
			name:        "Input Channel closed",
			ctx:         context.Background(),
			ch:          closedIn,
			wantErr:     true,
			wantStopper: true,
		},
		{
			name: "Bookmark event",
			ctx:  context.Background(),
			ch:   make(chan watch.Event, 1),
			event: watch.Event{
				Type: watch.Bookmark,
				Object: &fakeObject{
					rv: "1",
				},
			},
			wantRV: "1",
		},
		{
			name: "Event sent to cache",
			ctx:  context.Background(),
			ch:   make(chan watch.Event, 1),
			event: watch.Event{
				Type:   watch.Added,
				Object: &corev1.Pod{},
			},
			wantEvent: watch.Event{
				Type:   watch.Added,
				Object: &corev1.Pod{},
			},
		},
	}

	for _, test := range tests {
		r := &Reader{
			filterIn: make(chan watch.Event, 1),
		}
		if test.event != (watch.Event{}) {
			test.ch <- test.event
		}

		stopped := false
		stopper := newStopper(&stopped)

		gotRv, err := r.watchEvent(test.ctx, test.ch, stopper)
		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestWatchEvent(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("TestWatchEvent(%s): got err == %v, want err == nil", test.name, err)
			continue

		}
		if stopped != test.wantStopper {
			t.Errorf("TestWatchEvent(%s): got stopped == %v, want stopped == %v", test.name, stopped, test.wantStopper)
		}
		if gotRv != test.wantRV {
			t.Errorf("TestWatchEvent(%s): got rv == %s, want rv == %s", test.name, gotRv, test.wantRV)
		}
		if err != nil {
			continue
		}
		if !reflect.ValueOf(test.wantEvent).IsZero() {
			if diff := pretty.Compare(test.wantEvent, <-r.filterIn); diff != "" {
				t.Errorf("TestWatchEvent(%s): -want/+got:\n%s", test.name, diff)
			}
		}
	}

}
