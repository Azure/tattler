package watchlist

import (
	"errors"
	"log"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/readers/apiserver/watchlist/relist"
	"github.com/Azure/tattler/readers/apiserver/watchlist/types"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/retry/exponential"
	"github.com/gostdlib/base/values/generics/promises"
	"github.com/kylelemons/godebug/pretty"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
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

func init() {
	back = exponential.Must(exponential.New(exponential.WithTesting()))
}

// TestNew tests the New constructor
func TestNew(t *testing.T) {
	t.Parallel()

	clientset := fake.NewSimpleClientset()

	tests := []struct {
		name          string
		clientset     *fake.Clientset
		retrieveTypes types.Retrieve
		opts          []Option
		wantErr       bool
	}{
		{
			name:          "Success: valid clientset and retrieve types",
			clientset:     clientset,
			retrieveTypes: types.RTPod,
			wantErr:       false,
		},
		{
			name:          "Success: multiple retrieve types",
			clientset:     clientset,
			retrieveTypes: types.RTPod | types.RTNamespace,
			wantErr:       false,
		},
		{
			name:          "Success: with filter size option",
			clientset:     clientset,
			retrieveTypes: types.RTPod,
			opts:          []Option{WithFilterSize(1000)},
			wantErr:       false,
		},
		{
			name:          "Success: with relist option",
			clientset:     clientset,
			retrieveTypes: types.RTPod,
			opts:          []Option{WithRelist(2 * time.Hour)},
			wantErr:       false,
		},
		{
			name:          "Error: nil clientset",
			clientset:     nil,
			retrieveTypes: types.RTPod,
			wantErr:       true,
		},
		{
			name:          "Error: no retrieve types",
			clientset:     clientset,
			retrieveTypes: 0,
			wantErr:       true,
		},
		{
			name:          "Error: invalid retrieve types",
			clientset:     clientset,
			retrieveTypes: types.Retrieve(1 << 20), // Invalid bit
			wantErr:       true,
		},
		{
			name:          "Error: relist duration too short",
			clientset:     clientset,
			retrieveTypes: types.RTPod,
			opts:          []Option{WithRelist(30 * time.Minute)},
			wantErr:       true,
		},
		{
			name:          "Error: relist duration too long",
			clientset:     clientset,
			retrieveTypes: types.RTPod,
			opts:          []Option{WithRelist(8 * 24 * time.Hour)},
			wantErr:       true,
		},
	}

	for _, test := range tests {
		var clientsetInterface kubernetes.Interface
		if test.clientset != nil {
			clientsetInterface = test.clientset
		}
		reader, err := New(context.Background(), clientsetInterface, test.retrieveTypes, test.opts...)

		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestNew(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("TestNew(%s): got err == %v, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		if reader == nil {
			t.Errorf("TestNew(%s): got reader == nil, want reader != nil", test.name)
			continue
		}

		if reader.clientset == nil {
			t.Errorf("TestNew(%s): got clientset == nil, want clientset != nil", test.name)
		}

		if reader.retrieveTypes != test.retrieveTypes {
			t.Errorf("TestNew(%s): got retrieveTypes == %v, want retrieveTypes == %v", test.name, reader.retrieveTypes, test.retrieveTypes)
		}

		if reader.relister == nil {
			t.Errorf("TestNew(%s): got relister == nil, want relister != nil", test.name)
		}
	}
}

// TestClose tests the Close method
func TestClose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		started bool
		wantErr bool
	}{
		{
			name:    "Success: close after start",
			started: true,
			wantErr: false,
		},
		{
			name:    "Error: close before start",
			started: false,
			wantErr: true,
		},
	}

	for _, test := range tests {
		r := &Reader{
			started:  test.started,
			closeCh:  make(chan struct{}),
			filterIn: make(chan watch.Event, 1),
		}

		if test.started {
			_, cancel := context.WithCancel(context.Background())
			r.cancelWatches = cancel
			defer cancel()
		}

		err := r.Close(context.Background())

		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestClose(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("TestClose(%s): got err == %v, want err == nil", test.name, err)
			continue
		}
	}
}

// TestSetOut tests the SetOut method
func TestSetOut(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		started bool
		out     chan data.Entry
		wantErr bool
	}{
		{
			name:    "Success: set output channel",
			started: false,
			out:     make(chan data.Entry, 1),
			wantErr: false,
		},
		{
			name:    "Error: already started",
			started: true,
			out:     make(chan data.Entry, 1),
			wantErr: true,
		},
	}

	for _, test := range tests {
		r := &Reader{
			started: test.started,
		}

		err := r.SetOut(context.Background(), test.out)

		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestSetOut(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("TestSetOut(%s): got err == %v, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		if !test.wantErr && r.ch != test.out {
			t.Errorf("TestSetOut(%s): channel was not set correctly", test.name)
		}
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	log.Println("got here")

	watchesCalled := []types.Retrieve{}

	tests := []struct {
		name          string
		started       bool
		ch            chan data.Entry
		retrieves     types.Retrieve
		cancelWatcher bool
		fakeWatch     func(context.Context, types.Retrieve, []spawnWatcher) error
		wantRetrieves []types.Retrieve
		wantErr       bool
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
			name:      "Error: Namespace watch returns error",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTNamespace,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return errors.New("error")
			},
			wantRetrieves: []types.Retrieve{types.RTNamespace},
			wantErr:       true,
		},
		{
			name:      "Error: PersistentVolume watch returns error",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTPersistentVolume,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return errors.New("error")
			},
			wantRetrieves: []types.Retrieve{types.RTPersistentVolume},
			wantErr:       true,
		},
		{
			name:      "Error: Node watch returns error",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTNode,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return errors.New("error")
			},
			wantRetrieves: []types.Retrieve{types.RTNode},
			wantErr:       true,
		},
		{
			name:      "Namespace success",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTNamespace,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				log.Println(watchesCalled)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTNamespace},
		},
		{
			name:      "PersistentVolume success",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTPersistentVolume,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTPersistentVolume},
		},
		{
			name:      "Node success",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTNode,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTNode},
		},
		{
			name:      "Pod success",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTPod,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTPod},
		},
		{
			name:      "RBAC success",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTRBAC,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTRBAC},
		},
		{
			name:      "Services success",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTService,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTService},
		},
		{
			name:      "Deployments success",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTDeployment,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTDeployment},
		},
		{
			name:      "Ingress Controller success",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTIngressController,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTIngressController},
		},
		{
			name:      "Endpoint success",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTEndpoint,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTEndpoint},
		},
		{
			name: "All success",
			ch:   make(chan data.Entry, 1),
			retrieves: (types.RTNamespace | types.RTPersistentVolume | types.RTNode | types.RTPod | types.RTRBAC | types.RTService | types.RTDeployment |
				types.RTIngressController | types.RTEndpoint),
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spawnWatchers []spawnWatcher) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{
				types.RTNamespace,
				types.RTPersistentVolume,
				types.RTNode,
				types.RTPod,
				types.RTRBAC,
				types.RTService,
				types.RTDeployment,
				types.RTIngressController,
				types.RTEndpoint,
			},
		},
	}

	for _, test := range tests {
		watchesCalled = nil

		clientset := fake.NewSimpleClientset()
		relister, err := relist.New(clientset)
		if err != nil {
			t.Fatalf("TestRun(%s): failed to create relister: %v", test.name, err)
		}

		r := &Reader{
			started:       test.started,
			ch:            test.ch,
			retrieveTypes: test.retrieves,
			fakeWatch:     test.fakeWatch,
			clientset:     clientset,
			relister:      relister,
			closeCh:       make(chan struct{}),
		}

		err = r.Run(context.Background())
		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestRun(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			if strings.Contains(err.Error(), "Bug") {
				t.Logf("TestRun(%s): you have probably forgotten to do go generate ./...", test.name)
			}
			t.Errorf("TestRun(%s): got err == %v, want err == nil", test.name, err)
			continue
		}

		slices.Sort[[]types.Retrieve, types.Retrieve](watchesCalled)
		slices.Sort[[]types.Retrieve, types.Retrieve](test.wantRetrieves)
		if diff := pretty.Compare(test.wantRetrieves, watchesCalled); diff != "" {
			t.Errorf("TestRun(%s): types.Retrieves: -want/+got:\n%s", test.name, diff)
		}
	}
}

func TestSetupCache(t *testing.T) {
	t.Parallel()

	r := &Reader{
		ch: make(chan data.Entry, 1),
	}
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
}

// Note that this stack overflows if -race is used. See individual tests for more information.
// The test passes and this does not happen in production.
func TestWatch(t *testing.T) {
	t.Parallel()

	doneCtx, cancel := context.WithCancel(context.Background())
	cancel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eventWatcherCount := 0

	tests := []struct {
		name          string
		ctx           context.Context
		spawnWatchers []spawnWatcher
		eventWatcher  func(ctx context.Context, watcher watch.Interface) (string, error)
		wantErr       bool
	}{
		{
			name: "Context done",
			ctx:  doneCtx,
		},
		{
			name: "Watching had connection error and we haven't connected before",
			ctx:  ctx,
			spawnWatchers: []spawnWatcher{
				func(options metav1.ListOptions) (watch.Interface, error) {
					return nil, errors.New("error")
				},
			},
			wantErr: true,
		},
		{
			name: "Watching had connection error but we have connected before",
			ctx:  ctx,
			spawnWatchers: []spawnWatcher{
				func(options metav1.ListOptions) (watch.Interface, error) {
					return struct{ watch.Interface }{}, nil
				},
			},
			eventWatcher: func(ctx context.Context, watcher watch.Interface) (string, error) {
				if eventWatcherCount == 0 {
					eventWatcherCount++
					return "", nil
				}
				return "", errors.New("error")
			},
		},
	}

	for _, test := range tests {
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

		err := r.watch(ctx, types.RTNamespace, test.spawnWatchers)
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

func TestRelistTask(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		relistInterval time.Duration
		wantCalled     bool
	}{
		{
			name:           "relist disabled",
			relistInterval: -1,
			wantCalled:     true, // testHandleClientSwitch is always called for testing
		},
		{
			name:           "relist enabled",
			relistInterval: 1 * time.Hour,
			wantCalled:     true,
		},
	}

	for _, test := range tests {
		handleClientSwitchCalled := false
		r := &Reader{
			relistInterval: test.relistInterval,
			testHandleClientSwitch: func() {
				handleClientSwitchCalled = true
			},
		}

		r.relistTask(context.Background())

		if handleClientSwitchCalled != test.wantCalled {
			t.Errorf("TestRelistTask(%s): handleClientSwitch called = %v, want %v", test.name, handleClientSwitchCalled, test.wantCalled)
		}
	}
}

// TestPerformRelist tests the performRelist functionality
func TestPerformRelist(t *testing.T) {
	t.Parallel()

	// Create a fake clientset with some pods
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-1",
			Namespace: "default",
			UID:       k8stypes.UID("uid-1"),
		},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-2",
			Namespace: "default",
			UID:       k8stypes.UID("uid-2"),
		},
	}

	clientset := fake.NewSimpleClientset(pod1, pod2)

	tests := []struct {
		name          string
		retrieves     types.Retrieve
		setupRelister func() (*relist.Relister, error)
		setupChannel  func() chan data.Entry
		ctxFunc       func() context.Context
		wantErr       bool
		wantEntries   int
	}{
		{
			name:      "successful relist for pods",
			retrieves: types.RTPod,
			setupRelister: func() (*relist.Relister, error) {
				return relist.New(clientset)
			},
			setupChannel: func() chan data.Entry {
				return make(chan data.Entry, 10)
			},
			ctxFunc: func() context.Context {
				return context.Background()
			},
			wantErr:     false,
			wantEntries: 2,
		},
		{
			name:      "context cancelled",
			retrieves: types.RTPod,
			setupRelister: func() (*relist.Relister, error) {
				return relist.New(clientset)
			},
			setupChannel: func() chan data.Entry {
				return make(chan data.Entry, 10)
			},
			ctxFunc: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantErr:     true,
			wantEntries: 0,
		},
		{
			name:      "multiple resource types",
			retrieves: types.RTPod | types.RTNamespace,
			setupRelister: func() (*relist.Relister, error) {
				return relist.New(clientset)
			},
			setupChannel: func() chan data.Entry {
				return make(chan data.Entry, 10)
			},
			ctxFunc: func() context.Context {
				return context.Background()
			},
			wantErr:     false,
			wantEntries: 2, // Only pods in the fake clientset
		},
	}

	for _, test := range tests {
		relister, err := test.setupRelister()
		if err != nil {
			t.Fatalf("TestPerformRelist(%s): failed to setup relister: %v", test.name, err)
		}

		ch := test.setupChannel()
		ctx := test.ctxFunc()

		r := &Reader{
			retrieveTypes:  test.retrieves,
			relister:       relister,
			ch:             ch,
			relistInterval: 1 * time.Hour,
		}

		// Run performRelist in a goroutine
		done := make(chan error, 1)
		go func() {
			done <- r.performRelist(ctx)
		}()

		// Collect entries from the channel, waiting for expected count or timeout
		var entries []data.Entry
		collectTimeout := time.After(2 * time.Second)
		methodDone := false

	collectLoop:
		for {
			select {
			case entry := <-ch:
				entries = append(entries, entry)
				// If we got all expected entries and the method is done, break
				if len(entries) == test.wantEntries && methodDone {
					break collectLoop
				}
			case err := <-done:
				methodDone = true
				if test.wantErr && err == nil {
					t.Errorf("TestPerformRelist(%s): expected error but got none", test.name)
					break collectLoop
				}
				if !test.wantErr && err != nil {
					t.Errorf("TestPerformRelist(%s): expected no error but got: %v", test.name, err)
					break collectLoop
				}
				// If we already got all expected entries, break
				if len(entries) == test.wantEntries {
					break collectLoop
				}
				// Otherwise keep collecting for a bit more
			case <-collectTimeout:
				break collectLoop
			}
		}

		if len(entries) != test.wantEntries {
			t.Errorf("TestPerformRelist(%s): got %d entries, want %d", test.name, len(entries), test.wantEntries)
		}

		// Verify entries are snapshots
		for i, entry := range entries {
			if entry.ChangeType() != data.CTSnapshot {
				t.Errorf("TestPerformRelist(%s): entry[%d].ChangeType() = %v, want %v", test.name, i, entry.ChangeType(), data.CTSnapshot)
			}
		}
	}
}

// TestPerformRelistWithError tests error handling in performRelist
func TestPerformRelistWithError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ch := make(chan data.Entry, 10)

	// Create a mock relister that returns an error
	mockRelister := &mockRelister{
		listFunc: func(ctx context.Context, rt types.Retrieve) (chan promises.Response[data.Entry], error) {
			respCh := make(chan promises.Response[data.Entry], 1)
			go func() {
				defer close(respCh)
				respCh <- promises.Response[data.Entry]{Err: errors.New("list error")}
			}()
			return respCh, nil
		},
	}

	r := &Reader{
		retrieveTypes: types.RTPod,
		relister:      mockRelister,
		ch:            ch,
	}

	// performRelist should log the error but not return it
	err := r.performRelist(ctx)
	if err != nil {
		t.Errorf("TestPerformRelistWithError: performRelist should not return error for list failures, got: %v", err)
	}
}

// mockRelister is a mock implementation of the Relister interface
type mockRelister struct {
	listFunc func(ctx context.Context, rt types.Retrieve) (chan promises.Response[data.Entry], error)
}

func (m *mockRelister) List(ctx context.Context, rt types.Retrieve) (chan promises.Response[data.Entry], error) {
	if m.listFunc != nil {
		return m.listFunc(ctx, rt)
	}
	return nil, errors.New("not implemented")
}
