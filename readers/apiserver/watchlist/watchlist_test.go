package watchlist

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store/configmap"
	storefake "github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store/fake"
	"github.com/Azure/tattler/readers/apiserver/watchlist/internal/apierr"
	"github.com/Azure/tattler/readers/apiserver/watchlist/relist"
	"github.com/Azure/tattler/readers/apiserver/watchlist/types"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/errors"
	"github.com/gostdlib/base/retry/exponential"
	"github.com/gostdlib/base/values/chans"
	"github.com/gostdlib/base/values/generics/promises"
	"github.com/kylelemons/godebug/pretty"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/apimachinery/pkg/watch"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
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

var namespaceGVR = corev1.SchemeGroupVersion.WithResource("namespaces")

func init() {
	back = exponential.Must(exponential.New(exponential.WithTesting()))
	bookmarkCheckBack = exponential.Must(exponential.New(exponential.WithPolicy(exponential.SecondsRetryPolicy()), exponential.WithTesting()))
}

// TestNew tests the New constructor
func TestNew(t *testing.T) {
	t.Parallel()

	clientset := fake.NewSimpleClientset()
	bookmarkStore, err := configmap.New(clientset, "default", "tattler-bookmarks")
	if err != nil {
		t.Fatalf("configmap.New() got err %v, want nil", err)
	}

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
			name:          "Success: with relist option",
			clientset:     clientset,
			retrieveTypes: types.RTPod,
			opts:          []Option{WithRelist(2 * time.Hour)},
			wantErr:       false,
		},
		{
			name:          "Success: with bookmark store option",
			clientset:     clientset,
			retrieveTypes: types.RTPod,
			opts:          []Option{WithBookmarkStore(bookmarkStore)},
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
			name:          "Error: a known retrieve type combined with an unknown bit",
			clientset:     clientset,
			retrieveTypes: types.RTPod | types.Retrieve(1<<20),
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
		reader, err := New(t.Context(), clientsetInterface, test.retrieveTypes, test.opts...)

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
		// watcherErr is returned by a watcher running when Close is called.
		watcherErr error
		wantErr    bool
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
		{
			name:       "Error: a watcher stopped with an error",
			started:    true,
			watcherErr: errors.New("watcher failed"),
			wantErr:    true,
		},
	}

	for _, test := range tests {
		didCancel := false
		r := &Reader{
			started: test.started,
			closeCh: make(chan struct{}),
			dataCh:  make(chan data.Entry, 1),
			cancel:  func() { didCancel = true },
			spawnCh: make(chan promises.Value[spawnWatcher, watch.Interface]),
		}

		if test.started {
			_, cancel := context.WithCancel(t.Context())
			r.cancelWatches = cancel
			defer cancel()
		}
		r.waitWatchers.Go(t.Context(), func(context.Context) error { return test.watcherErr })

		err := r.Close(t.Context())

		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestClose(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("TestClose(%s): got err == %v, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}
		if !didCancel {
			t.Errorf("TestClose(%s): cancel was not called", test.name)
		}
		if _, _, closed := chans.TryGet(r.closeCh); !closed {
			t.Errorf("TestClose(%s): closeCh was not closed", test.name)
		}
		if _, _, closed := chans.TryGet(r.spawnCh); !closed {
			t.Errorf("TestClose(%s): spawnCh was not closed", test.name)
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

		err := r.SetOut(t.Context(), test.out)

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

		if !test.wantErr && r.dataCh != test.out {
			t.Errorf("TestSetOut(%s): channel was not set correctly", test.name)
		}
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	watchesCalled := []types.Retrieve{}

	tests := []struct {
		name          string
		started       bool
		ch            chan data.Entry
		retrieves     types.Retrieve
		cancelWatcher bool
		fakeWatch     func(context.Context, types.Retrieve, []watchSpec) error
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
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
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
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
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
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
				watchesCalled = append(watchesCalled, rt)
				return errors.New("error")
			},
			wantRetrieves: []types.Retrieve{types.RTNode},
			wantErr:       true,
		},
		{
			name:      "Success: starts the namespace watcher",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTNamespace,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTNamespace},
		},
		{
			name:      "Success: starts the persistent volume watcher",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTPersistentVolume,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTPersistentVolume},
		},
		{
			name:      "Success: starts the node watcher",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTNode,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTNode},
		},
		{
			name:      "Success: starts the pod watcher",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTPod,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTPod},
		},
		{
			name:      "Success: starts the RBAC watcher",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTRBAC,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTRBAC},
		},
		{
			name:      "Success: starts the service watcher",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTService,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTService},
		},
		{
			name:      "Success: starts the deployment watcher",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTDeployment,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTDeployment},
		},
		{
			name:      "Success: starts the ingress watcher",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTIngressController,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTIngressController},
		},
		{
			name:      "Success: starts the endpoint watcher",
			ch:        make(chan data.Entry, 1),
			retrieves: types.RTEndpoint,
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
				watchesCalled = append(watchesCalled, rt)
				return nil
			},
			wantRetrieves: []types.Retrieve{types.RTEndpoint},
		},
		{
			name: "Success: starts every watcher",
			ch:   make(chan data.Entry, 1),
			retrieves: (types.RTNamespace | types.RTPersistentVolume | types.RTNode | types.RTPod | types.RTRBAC | types.RTService | types.RTDeployment |
				types.RTIngressController | types.RTEndpoint),
			fakeWatch: func(ctx context.Context, rt types.Retrieve, spanWatchers []watchSpec) error {
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
			dataCh:        test.ch,
			retrieveTypes: test.retrieves,
			fakeWatch:     test.fakeWatch,
			clientset:     clientset,
			relister:      relister,
			closeCh:       make(chan struct{}),
		}

		err = r.Run(t.Context())
		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestRun(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
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

func TestRunFailureStopsStartedWatchers(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// RBAC starts roles, rolebindings, clusterroles and clusterrolebindings in that order. The first clusterroles watch
	// is forbidden, so Run fails after the roles and rolebindings watchers are already running. Reactors are only
	// called from connectWatcher, one at a time, so the counter needs no lock.
	var started []*watch.FakeWatcher
	clusterRoleCalls := 0
	clientset := fake.NewSimpleClientset()
	clientset.PrependWatchReactor("*", func(action k8stesting.Action) (bool, watch.Interface, error) {
		if action.GetResource().Resource == "clusterroles" {
			clusterRoleCalls++
			if clusterRoleCalls == 1 {
				return true, nil, apierrors.NewForbidden(action.GetResource().GroupResource(), "", errors.New("rbac"))
			}
		}
		w := watch.NewFake()
		started = append(started, w)
		return true, w, nil
	})

	r := &Reader{
		clientset:         clientset,
		retrieveTypes:     types.RTRBAC,
		relistInterval:    -1,
		cancel:            func() {},
		closeCh:           make(chan struct{}),
		spawnCh:           make(chan promises.Value[spawnWatcher, watch.Interface]),
		watcherSpawnDelay: 1 * time.Millisecond,
		dataCh:            make(chan data.Entry, 1),
	}
	context.Pool(ctx).Submit(ctx, func() { r.connectWatcher(ctx, r.spawnCh) })

	if err := r.Run(ctx); err == nil {
		t.Fatalf("TestRunFailureStopsStartedWatchers: first Run() got err == nil, want err != nil")
	}
	if len(started) != 2 {
		t.Fatalf("TestRunFailureStopsStartedWatchers: got %d watchers started before the failure, want 2", len(started))
	}
	// Run waits for the watchers it started to exit before returning its error, so they must already be stopped. This
	// is fatal because Close() below would wait forever on watchers that were left running.
	for i, w := range started {
		if !w.IsStopped() {
			t.Fatalf("TestRunFailureStopsStartedWatchers: watcher %d is still running after Run() failed", i)
		}
	}

	// The failed Run must leave the Reader able to run again.
	if err := r.Run(ctx); err != nil {
		t.Fatalf("TestRunFailureStopsStartedWatchers: second Run() got err == %s, want err == nil", err)
	}
	if len(started) != 6 {
		t.Fatalf("TestRunFailureStopsStartedWatchers: got %d watchers started in total, want 6", len(started))
	}
	if err := r.Close(ctx); err != nil {
		t.Fatalf("TestRunFailureStopsStartedWatchers: Close() got err == %s, want err == nil", err)
	}
	for i, w := range started {
		if !w.IsStopped() {
			t.Errorf("TestRunFailureStopsStartedWatchers: watcher %d is still running after Close()", i)
		}
	}
}

type watcherWithOnlyStop struct {
	watch.Interface
}

func (w watcherWithOnlyStop) Stop() {}

// Note that this stack overflows if -race is used. See individual tests for more information.
// The test passes and this does not happen in production.
func TestWatch(t *testing.T) {
	t.Parallel()

	doneCtx, cancel := context.WithCancel(t.Context())
	cancel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	eventWatcherCount := 0

	tests := []struct {
		name         string
		ctx          context.Context
		spanWatchers []watchSpec
		eventWatcher func(ctx context.Context, watcher watch.Interface) (string, error)
		wantErr      bool
	}{
		{
			name: "Success: a done context returns without starting watchers",
			ctx:  doneCtx,
		},
		{
			name: "Error: the first watcher cannot connect",
			ctx:  ctx,
			spanWatchers: []watchSpec{
				{spawn: func(options metav1.ListOptions) (watch.Interface, error) {
					return nil, errors.New("error")
				}},
			},
			wantErr: true,
		},
		{
			name: "Success: a watcher that connected before reconnects after an error",
			ctx:  ctx,
			spanWatchers: []watchSpec{
				{spawn: func(options metav1.ListOptions) (watch.Interface, error) {
					return watcherWithOnlyStop{}, nil
				}},
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
			fakeWatchEvents:   test.eventWatcher,
			spawnCh:           make(chan promises.Value[spawnWatcher, watch.Interface]),
			watcherSpawnDelay: 10 * time.Millisecond, // Use shorter delay for tests
		}

		var ctx context.Context
		if test.ctx.Err() == nil {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(t.Context())

			// Start the connectWatcher goroutine to process spawn requests
			context.Pool(t.Context()).Submit(t.Context(), func() { r.connectWatcher(ctx, r.spawnCh) })

			time.AfterFunc(100*time.Millisecond, cancel)
		} else {
			ctx = test.ctx
		}

		err := r.watch(ctx, types.RTNamespace, test.spanWatchers)
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

func TestCreateWatcherBookmarkKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		create  func(*Reader, context.Context) []watchSpec
		wantKey []string
	}{
		{name: "Success: namespace", create: (*Reader).createNamespaceWatcher, wantKey: []string{"core.v1.namespaces"}},
		{name: "Success: node", create: (*Reader).createNodesWatcher, wantKey: []string{"core.v1.nodes"}},
		{name: "Success: pod", create: (*Reader).createPodsWatcher, wantKey: []string{"core.v1.pods"}},
		{name: "Success: persistent volume", create: (*Reader).createPersistentVolumesWatcher, wantKey: []string{"core.v1.persistentvolumes"}},
		{name: "Success: rbac", create: (*Reader).createRBACWatcher, wantKey: []string{
			"rbac.authorization.k8s.io.v1.roles",
			"rbac.authorization.k8s.io.v1.rolebindings",
			"rbac.authorization.k8s.io.v1.clusterroles",
			"rbac.authorization.k8s.io.v1.clusterrolebindings",
		}},
		{name: "Success: service", create: (*Reader).createServicesWatcher, wantKey: []string{"core.v1.services"}},
		{name: "Success: deployment", create: (*Reader).createDeploymentsWatcher, wantKey: []string{"apps.v1.deployments"}},
		{name: "Success: ingress", create: (*Reader).createIngressesWatcher, wantKey: []string{"networking.k8s.io.v1.ingresses"}},
		{name: "Success: endpoint", create: (*Reader).createEndpointsWatcher, wantKey: []string{"core.v1.endpoints"}},
	}

	for _, test := range tests {
		watchers := test.create(&Reader{clientset: fake.NewSimpleClientset()}, t.Context())
		gotKey := make([]string, 0, len(watchers))
		for _, watcher := range watchers {
			gotKey = append(gotKey, bookmarkName(watcher.gvr))
		}
		if diff := pretty.Compare(test.wantKey, gotKey); diff != "" {
			t.Errorf("TestCreateWatcherBookmarkKeys(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}

func TestWatchBookmarkStoreStartup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		storedRV      string
		loadErr       error
		watchErr      error
		replacementRV string
		deleteErr     error
		// listErr makes the server's current resourceVersion unavailable; the list otherwise reports version 600.
		listErr error
		// noWatchList makes the server reject WatchList options, as one without the WatchList feature gate does.
		noWatchList bool
		wantRVs     []string
		wantDelete  bool
		wantErr     bool
		// wantRecovery is set when a rejected bookmark must be recovered with an ordered snapshot/watch stream.
		wantRecovery                  bool
		wantRecoverySendInitialEvents *bool
	}{
		{
			name:     "Success: uses stored bookmark on startup",
			storedRV: "500",
			wantRVs:  []string{"500"},
		},
		{
			name:     "Success: a bookmark at the server's current version resumes from it",
			storedRV: "600",
			wantRVs:  []string{"600"},
		},
		{
			name:                          "Success: a bookmark ahead of the server is cleared and the watch starts in recovery",
			storedRV:                      "700",
			wantRVs:                       []string{""},
			wantDelete:                    true,
			wantRecovery:                  true,
			wantRecoverySendInitialEvents: ptr.To(true),
		},
		{
			name:     "Success: resumes from the bookmark when the server's current version cannot be listed",
			storedRV: "700",
			listErr:  errors.New("list unavailable"),
			wantRVs:  []string{"700"},
		},
		{
			name:                          "Success: clears expired bookmark and streams ordered recovery snapshots",
			storedRV:                      "500",
			watchErr:                      apierrors.NewResourceExpired("the resourceVersion is too old"),
			wantRVs:                       []string{"500", ""},
			wantDelete:                    true,
			wantRecovery:                  true,
			wantRecoverySendInitialEvents: ptr.To(true),
		},
		{
			name:                          "Success: clears malformed bookmark and streams ordered recovery snapshots",
			storedRV:                      "malformed",
			watchErr:                      apierrors.NewBadRequest("invalid resourceVersion"),
			wantRVs:                       []string{"malformed", ""},
			wantDelete:                    true,
			wantRecovery:                  true,
			wantRecoverySendInitialEvents: ptr.To(true),
		},
		{
			name:                          "Success: leaves another reader's replacement bookmark in place and recovers instead of resuming from it",
			storedRV:                      "100",
			watchErr:                      apierrors.NewResourceExpired("the resourceVersion is too old"),
			replacementRV:                 "500",
			wantRVs:                       []string{"100", ""},
			wantRecovery:                  true,
			wantRecoverySendInitialEvents: ptr.To(true),
		},
		{
			name:        "Success: without WatchList support an expired bookmark watches from the recovery list version",
			storedRV:    "500",
			watchErr:    apierrors.NewResourceExpired("the resourceVersion is too old"),
			noWatchList: true,
			// The first spawn is rejected for its WatchList options and retried as a standard watch.
			wantRVs:                       []string{"500", "500", "600"},
			wantDelete:                    true,
			wantRecovery:                  true,
			wantRecoverySendInitialEvents: nil,
		},
		{
			name:     "Error: preserves bookmark after transient startup failure",
			storedRV: "500",
			watchErr: errors.New("temporary API outage"),
			wantRVs:  []string{"500"},
			wantErr:  true,
		},
		{
			name:                          "Success: a failed bookmark clear still starts the watch in recovery",
			storedRV:                      "500",
			watchErr:                      apierrors.NewResourceExpired("the resourceVersion is too old"),
			deleteErr:                     errors.New("bookmark ConfigMap update failed"),
			wantRVs:                       []string{"500", ""},
			wantRecovery:                  true,
			wantRecoverySendInitialEvents: ptr.To(true),
		},
		{
			name:    "Success: ignores bookmark load error",
			loadErr: errors.New("bookmark ConfigMap not found"),
			wantRVs: []string{""},
		},
	}

	for _, test := range tests {
		func() {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			key := namespaceGVR
			store := storefake.New(map[schema.GroupVersionResource]string{key: test.storedRV})
			store.SetLoadError(test.loadErr)
			store.SetDeleteError(test.deleteErr)
			capturedOptions := []metav1.ListOptions{}
			relisted := make(chan types.Retrieve, 1)
			// watching receives once a watch starts, as recovery without WatchList starts it after watch() returns.
			watching := make(chan struct{}, 1)

			r := &Reader{
				spawnCh:           make(chan promises.Value[spawnWatcher, watch.Interface]),
				watcherSpawnDelay: 1 * time.Millisecond,
				bookmarking:       true,
				bookmarkStore:     store,
				relister:          newRecordingRelister(relisted),
				dataCh:            make(chan data.Entry, 1),
				fakeWatchEvents: func(ctx context.Context, watcher watch.Interface) (string, error) {
					<-ctx.Done()
					return "", nil
				},
			}

			context.Pool(t.Context()).Submit(t.Context(), func() { r.connectWatcher(ctx, r.spawnCh) })

			sp := func(options metav1.ListOptions) (watch.Interface, error) {
				capturedOptions = append(capturedOptions, options)
				if test.noWatchList && options.SendInitialEvents != nil {
					return nil, watchListGateError()
				}
				if test.watchErr != nil && options.ResourceVersion == test.storedRV {
					if test.replacementRV != "" {
						if err := store.Store(ctx, key, test.replacementRV); err != nil {
							t.Errorf("TestWatchBookmarkStoreStartup(%s): got Store() err == %v, want err == nil", test.name, err)
							return nil, err
						}
					}
					return nil, test.watchErr
				}
				chans.TryPut(watching, struct{}{})
				return watcherWithOnlyStop{}, nil
			}

			err := r.watch(ctx, types.RTNamespace, []watchSpec{{
				gvr: key, spawn: sp,
				list: func(context.Context, metav1.ListOptions) (runtime.Object, error) {
					if test.listErr != nil {
						return nil, test.listErr
					}
					return &corev1.NamespaceList{ListMeta: metav1.ListMeta{ResourceVersion: "600"}}, nil
				},
			}})
			switch {
			case err == nil && test.wantErr:
				t.Errorf("TestWatchBookmarkStoreStartup(%s): got err == nil, want err != nil", test.name)
				return
			case err != nil && !test.wantErr:
				t.Errorf("TestWatchBookmarkStoreStartup(%s): got err == %s, want err == nil", test.name, err)
				return
			}
			if !test.wantErr {
				waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
				_, res := chans.Get(waitCtx, watching)
				waitCancel()
				if !res.OK() {
					t.Errorf("TestWatchBookmarkStoreStartup(%s): got watch start result %v, want the watch to start", test.name, res)
					return
				}
			}

			cancel()
			_ = r.waitWatchers.Wait(t.Context())
			if _, ok, _ := chans.TryGet(relisted); ok {
				t.Errorf("TestWatchBookmarkStoreStartup(%s): got an independent relist, want ordered recovery only", test.name)
			}

			if len(capturedOptions) != len(test.wantRVs) {
				t.Errorf("TestWatchBookmarkStoreStartup(%s): got %d spawn calls, want %d", test.name, len(capturedOptions), len(test.wantRVs))
				return
			}
			for i, wantRV := range test.wantRVs {
				if capturedOptions[i].ResourceVersion != wantRV {
					t.Errorf("TestWatchBookmarkStoreStartup(%s): spawn %d got ResourceVersion == %q, want %q", test.name, i, capturedOptions[i].ResourceVersion, wantRV)
				}
			}
			if test.wantRecovery {
				last := capturedOptions[len(capturedOptions)-1]
				if diff := pretty.Compare(test.wantRecoverySendInitialEvents, last.SendInitialEvents); diff != "" {
					t.Errorf("TestWatchBookmarkStoreStartup(%s): recovery watch SendInitialEvents: -want/+got:\n%s", test.name, diff)
				}
			}
			if gotDelete := store.Deletes().Len() > 0; gotDelete != test.wantDelete {
				t.Errorf("TestWatchBookmarkStoreStartup(%s): got bookmark delete == %v, want %v", test.name, gotDelete, test.wantDelete)
			}
			if test.replacementRV != "" {
				if got := store.Stored(key); got != test.replacementRV {
					t.Errorf("TestWatchBookmarkStoreStartup(%s): got stored bookmark %q, want the replacement %q left in place", test.name, got, test.replacementRV)
				}
			}
		}()
	}
}

// newRecordingRelister returns a relister whose List reports each relisted type on relisted and lists nothing.
func newRecordingRelister(relisted chan types.Retrieve) *fakeRelister {
	return &fakeRelister{
		listFunc: func(ctx context.Context, rt types.Retrieve) (chan promises.Response[data.Entry], error) {
			chans.TryPut(relisted, rt)
			ch := make(chan promises.Response[data.Entry])
			close(ch)
			return ch, nil
		},
	}
}

// watchListGateError is the error a server without the WatchList feature gate returns for WatchList options.
func watchListGateError() error {
	return apierrors.NewInvalid(
		schema.GroupKind{Group: "meta.k8s.io", Kind: "ListOptions"},
		"",
		field.ErrorList{field.Forbidden(field.NewPath("sendInitialEvents"), "sendInitialEvents is forbidden for watch unless the WatchList feature gate is enabled")},
	)
}

func TestWatchBookmarkInvalidWatchListOptionsFallback(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	capturedOptions := []metav1.ListOptions{}
	r := &Reader{
		spawnCh:           make(chan promises.Value[spawnWatcher, watch.Interface]),
		watcherSpawnDelay: 1 * time.Millisecond,
		bookmarking:       true,
		fakeWatchEvents: func(ctx context.Context, watcher watch.Interface) (string, error) {
			<-ctx.Done()
			return "", nil
		},
	}

	context.Pool(t.Context()).Submit(t.Context(), func() { r.connectWatcher(ctx, r.spawnCh) })

	sp := func(options metav1.ListOptions) (watch.Interface, error) {
		capturedOptions = append(capturedOptions, options)
		if options.SendInitialEvents != nil {
			return nil, apierrors.NewInvalid(
				schema.GroupKind{Group: "meta.k8s.io", Kind: "ListOptions"},
				"",
				field.ErrorList{field.Forbidden(field.NewPath("sendInitialEvents"), "sendInitialEvents is forbidden for watch unless the WatchList feature gate is enabled")},
			)
		}
		return watcherWithOnlyStop{}, nil
	}

	if err := r.watch(ctx, types.RTNamespace, []watchSpec{{gvr: namespaceGVR, spawn: sp}}); err != nil {
		t.Fatalf("watch returned error: %v", err)
	}
	cancel()

	if len(capturedOptions) != 2 {
		t.Fatalf("got %d spawn calls, want 2", len(capturedOptions))
	}
	if capturedOptions[0].SendInitialEvents == nil || capturedOptions[0].ResourceVersionMatch != metav1.ResourceVersionMatchNotOlderThan {
		t.Fatalf("first spawn options got %#v, want WatchList options", capturedOptions[0])
	}
	if capturedOptions[1].SendInitialEvents != nil || capturedOptions[1].ResourceVersionMatch != "" {
		t.Fatalf("fallback spawn options got %#v, want no WatchList options", capturedOptions[1])
	}
	if !capturedOptions[1].Watch || !capturedOptions[1].AllowWatchBookmarks {
		t.Fatalf("fallback spawn options got %#v, want watch with bookmarks", capturedOptions[1])
	}
}

func TestGetRecoveryWatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		reconnect         bool
		noWatchList       bool
		interruptRecovery bool
		wantListCalls     int
	}{
		{name: "Success: startup recovery orders WatchList snapshots before a concurrent deletion"},
		{name: "Success: reconnect recovery orders WatchList snapshots before a concurrent deletion", reconnect: true},
		{name: "Success: startup recovery replays a deletion from the delayed list version", noWatchList: true, wantListCalls: 2},
		{
			name:          "Success: reconnect recovery replays a deletion from the delayed list version",
			reconnect:     true,
			noWatchList:   true,
			wantListCalls: 2,
		},
		{name: "Success: interrupted initial events restart recovery without resuming from a premature bookmark", interruptRecovery: true},
	}
	for _, test := range tests {
		func() {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			key := corev1.SchemeGroupVersion.WithResource("pods")
			bookmarkStore := storefake.New(map[schema.GroupVersionResource]string{key: "100"})
			pod := func(name, rv string) *corev1.Pod {
				return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, UID: k8stypes.UID(name), ResourceVersion: rv}}
			}
			a := pod("a", "150")
			b := pod("b", "160")
			c := pod("c", "202")
			captured := make(chan struct{})
			release := make(chan struct{})
			live := watch.NewRaceFreeFake()
			clientset := fake.NewSimpleClientset()
			listCalls := 0
			clientset.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				options := action.(k8stesting.ListActionImpl).GetListOptions()
				if options.Limit == 1 {
					// The startup bookmark check lists one object for the server's current version; the stored
					// bookmark is behind it, so it is resumed from and then rejected as expired by the spawn.
					return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "200"}}, nil
				}
				listCalls++
				if options.Limit != recoveryListLimit {
					t.Errorf("TestGetRecoveryWatcher(%s): got list limit == %d, want %d", test.name, options.Limit, recoveryListLimit)
				}
				switch listCalls {
				case 1:
					if options.Continue != "" {
						t.Errorf("TestGetRecoveryWatcher(%s): got initial continue == %q, want empty", test.name, options.Continue)
					}
					return true, &corev1.PodList{
						ListMeta: metav1.ListMeta{ResourceVersion: "200", Continue: "next"},
						Items:    []corev1.Pod{*b},
					}, nil
				case 2:
					if options.Continue != "next" {
						t.Errorf("TestGetRecoveryWatcher(%s): got second continue == %q, want next", test.name, options.Continue)
					}
					// Capture A in the list, but hold the response until A has been deleted.
					page := &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "200"}, Items: []corev1.Pod{*a}}
					close(captured)
					if _, res := chans.Get(ctx, release); !res.Closed() {
						return true, nil, ctx.Err()
					}
					return true, page, nil
				default:
					return true, nil, errors.New("unexpected independent relist")
				}
			})

			r := &Reader{
				clientset:         clientset,
				bookmarking:       true,
				bookmarkStore:     bookmarkStore,
				spawnCh:           make(chan promises.Value[spawnWatcher, watch.Interface]),
				dataCh:            make(chan data.Entry),
				watcherSpawnDelay: time.Millisecond,
			}
			spec := r.createPodsWatcher(ctx)[0]
			resumeCalls := 0
			recoveryCalls := 0
			spec.spawn = func(options metav1.ListOptions) (watch.Interface, error) {
				if test.noWatchList && options.SendInitialEvents != nil {
					return nil, watchListGateError()
				}
				if options.ResourceVersion == "100" {
					resumeCalls++
					if test.reconnect && resumeCalls == 1 {
						old := watch.NewRaceFreeFake()
						old.Action(watch.Bookmark, pod("", "100"))
						old.Stop()
						return old, nil
					}
					return nil, apierrors.NewResourceExpired("expired bookmark")
				}
				if test.noWatchList {
					if options.ResourceVersion != "200" {
						t.Errorf("TestGetRecoveryWatcher(%s): got watch version == %q, want list version 200", test.name, options.ResourceVersion)
					}
					// These are changes retained by the server after the list's resource version.
					return live, nil
				}
				if options.ResourceVersion != "" || options.SendInitialEvents == nil || !*options.SendInitialEvents {
					t.Errorf("TestGetRecoveryWatcher(%s): got recovery options == %#v, want ordered initial events", test.name, options)
				}
				recoveryCalls++
				if test.interruptRecovery && recoveryCalls == 1 {
					partial := watch.NewRaceFreeFake()
					partial.Add(pod("partial", "175"))
					partial.Action(watch.Bookmark, pod("", "175"))
					partial.Stop()
					return partial, nil
				}
				live.Add(b)
				live.Add(a)
				boundary := pod("", "200")
				boundary.Annotations = map[string]string{metav1.InitialEventsAnnotationKey: "true"}
				live.Action(watch.Bookmark, boundary)
				close(captured)
				if _, res := chans.Get(ctx, release); !res.Closed() {
					live.Stop()
					return nil, ctx.Err()
				}
				return live, nil
			}

			spawner := context.Pool(ctx).Default().Group()
			startup := context.Pool(ctx).Default().Group()
			defer func() {
				cancel()
				cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
				defer cleanupCancel()
				_ = startup.Wait(cleanupCtx)
				_ = r.waitWatchers.Wait(cleanupCtx)
				close(r.spawnCh)
				_ = spawner.Wait(cleanupCtx)
				live.Stop()
			}()
			spawner.Go(context.WithoutCancel(ctx), func(context.Context) error {
				r.connectWatcher(ctx, r.spawnCh)
				return nil
			})
			startup.Go(context.WithoutCancel(ctx), func(context.Context) error {
				return r.watch(ctx, types.RTPod, []watchSpec{spec})
			})

			if test.interruptRecovery {
				entry, res := chans.Get(ctx, r.dataCh)
				if !res.OK() || entry.UID() != "partial" || entry.ChangeType() != data.CTSnapshot {
					t.Errorf("TestGetRecoveryWatcher(%s): got interrupted recovery entry == %v, result == %v, want partial snapshot", test.name, entry, res)
					return
				}
			}
			if _, res := chans.Get(ctx, captured); !res.Closed() {
				t.Errorf("TestGetRecoveryWatcher(%s): recovery did not capture its snapshot: %v", test.name, res)
				return
			}
			if test.noWatchList {
				// The list must run outside startup's watchdog and must not hold the spawner other watchers need.
				if err := startup.Wait(ctx); err != nil {
					t.Errorf("TestGetRecoveryWatcher(%s): got watch() err == %v while the recovery list is pending, want err == nil", test.name, err)
					return
				}
				w, err := r.getWatcher(ctx, types.RTNamespace, func(metav1.ListOptions) (watch.Interface, error) {
					return watcherWithOnlyStop{}, nil
				})
				if err != nil {
					t.Errorf("TestGetRecoveryWatcher(%s): got getWatcher() err == %v while the recovery list is pending, want err == nil", test.name, err)
					return
				}
				w.Stop()
			}
			if got, err := bookmarkStore.Load(ctx, key); err != nil || got != "" {
				t.Errorf("TestGetRecoveryWatcher(%s): got premature recovery bookmark == %q, err == %v, want empty and nil", test.name, got, err)
			}
			if _, ok, _ := chans.TryGet(r.dataCh); ok {
				t.Errorf("TestGetRecoveryWatcher(%s): snapshot delivered before delayed response was released", test.name)
				return
			}
			live.Delete(pod("a", "201"))
			live.Add(c)
			close(release)
			if err := startup.Wait(ctx); err != nil {
				t.Errorf("TestGetRecoveryWatcher(%s): got watch() err == %v, want err == nil", test.name, err)
				return
			}

			want := []data.Entry{
				data.MustNewEntry(b, data.STWatchList, data.CTSnapshot),
				data.MustNewEntry(a, data.STWatchList, data.CTSnapshot),
				data.MustNewEntry(pod("a", "201"), data.STWatchList, data.CTDelete),
				data.MustNewEntry(c, data.STWatchList, data.CTAdd),
			}
			present := map[k8stypes.UID]bool{}
			for i, expected := range want {
				entry, res := chans.Get(ctx, r.dataCh)
				if !res.OK() {
					t.Errorf("TestGetRecoveryWatcher(%s): got result %v for entry %d, want an entry", test.name, res, i)
					return
				}
				if diff := pretty.Compare(expected, entry); diff != "" {
					t.Errorf("TestGetRecoveryWatcher(%s): entry %d -want/+got:\n%s", test.name, i, diff)
				}
				switch entry.ChangeType() {
				case data.CTDelete:
					delete(present, entry.UID())
				default:
					present[entry.UID()] = true
				}
			}
			if present[a.UID] {
				t.Errorf("TestGetRecoveryWatcher(%s): deleted pod was resurrected by a stale snapshot", test.name)
			}
			if listCalls != test.wantListCalls {
				t.Errorf("TestGetRecoveryWatcher(%s): got %d list calls, want %d", test.name, listCalls, test.wantListCalls)
			}
		}()
	}
}

func TestListRecoveryObjects(t *testing.T) {
	t.Parallel()

	pod := func(name string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	// pages returns a lister that serves lists in order, one per call.
	pages := func(lists ...runtime.Object) func(context.Context, metav1.ListOptions) (runtime.Object, error) {
		call := 0
		return func(context.Context, metav1.ListOptions) (runtime.Object, error) {
			call++
			return lists[call-1], nil
		}
	}

	tests := []struct {
		name        string
		list        func(context.Context, metav1.ListOptions) (runtime.Object, error)
		wantObjects []runtime.Object
		wantRV      string
		wantErr     bool
		// wantPermanent is set when retrying the list cannot succeed, so the recovery must stop instead of retrying.
		wantPermanent bool
	}{
		{
			name: "Success: returns every page's objects at the list's resourceVersion",
			list: pages(
				&corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "200", Continue: "next"}, Items: []corev1.Pod{*pod("a")}},
				&corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "200"}, Items: []corev1.Pod{*pod("b")}},
			),
			wantObjects: []runtime.Object{pod("a"), pod("b")},
			wantRV:      "200",
		},
		{
			name:          "Error: no lister",
			wantErr:       true,
			wantPermanent: true,
		},
		{
			name: "Error: the list call fails",
			list: func(context.Context, metav1.ListOptions) (runtime.Object, error) {
				return nil, errors.New("list failed")
			},
			wantErr: true,
		},
		{
			name: "Error: the list call is forbidden",
			list: func(context.Context, metav1.ListOptions) (runtime.Object, error) {
				return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("rbac"))
			},
			wantErr:       true,
			wantPermanent: true,
		},
		{
			name:          "Error: the result is not a list",
			list:          pages(pod("a")),
			wantErr:       true,
			wantPermanent: true,
		},
		{
			name:          "Error: the list has no resourceVersion",
			list:          pages(&corev1.PodList{Items: []corev1.Pod{*pod("a")}}),
			wantErr:       true,
			wantPermanent: true,
		},
		{
			name: "Error: pages disagree on the resourceVersion",
			list: pages(
				&corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "200", Continue: "next"}, Items: []corev1.Pod{*pod("a")}},
				&corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "300"}, Items: []corev1.Pod{*pod("b")}},
			),
			wantErr:       true,
			wantPermanent: true,
		},
	}

	for _, test := range tests {
		objects, rv, err := listRecoveryObjects(t.Context(), watchSpec{gvr: corev1.SchemeGroupVersion.WithResource("pods"), list: test.list})
		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestListRecoveryObjects(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestListRecoveryObjects(%s): got err == %s, want err == nil", test.name, err)
			continue
		case err != nil:
			// handleWatcher's back.Retry (watchlist.go) stops retrying the recovery only on exponential.ErrPermanent,
			// so a list that retrying cannot fix must be permanent and a transient list failure must not be.
			if gotPermanent := errors.Is(err, exponential.ErrPermanent); gotPermanent != test.wantPermanent {
				t.Errorf("TestListRecoveryObjects(%s): got errors.Is(err, exponential.ErrPermanent) == %v, want %v: %v", test.name, gotPermanent, test.wantPermanent, err)
			}
			continue
		}
		if rv != test.wantRV {
			t.Errorf("TestListRecoveryObjects(%s): got resourceVersion == %q, want %q", test.name, rv, test.wantRV)
		}
		if diff := pretty.Compare(test.wantObjects, objects); diff != "" {
			t.Errorf("TestListRecoveryObjects(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}

func TestBookmarkAhead(t *testing.T) {
	t.Parallel()

	listAt := func(resourceVersion string) func(context.Context, metav1.ListOptions) (runtime.Object, error) {
		return func(context.Context, metav1.ListOptions) (runtime.Object, error) {
			return &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: resourceVersion}}, nil
		}
	}

	tests := []struct {
		name     string
		bookmark string
		list     func(context.Context, metav1.ListOptions) (runtime.Object, error)
		// timeout replaces the check's time budget; zero leaves a budget no row reaches.
		timeout   time.Duration
		wantAhead bool
		wantErr   bool
	}{
		{name: "Success: bookmark ahead of the server", bookmark: "200", list: listAt("100"), wantAhead: true},
		{name: "Success: bookmark at the server's version", bookmark: "200", list: listAt("200")},
		{name: "Success: bookmark behind the server", bookmark: "200", list: listAt("300")},
		{name: "Error: bookmark is not numeric", bookmark: "abc", list: listAt("100"), wantErr: true},
		{name: "Error: server version is not numeric", bookmark: "200", list: listAt("abc"), wantErr: true},
		{
			name:     "Error: the list call fails",
			bookmark: "200",
			list: func(context.Context, metav1.ListOptions) (runtime.Object, error) {
				return nil, errors.New("list unavailable")
			},
			wantErr: true,
		},
		{
			name:     "Error: the result is not a list",
			bookmark: "200",
			list: func(context.Context, metav1.ListOptions) (runtime.Object, error) {
				return &corev1.Pod{}, nil
			},
			wantErr: true,
		},
		{
			name:     "Error: the list does not answer within the check's time budget",
			bookmark: "200",
			list: func(ctx context.Context, _ metav1.ListOptions) (runtime.Object, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
			timeout: 20 * time.Millisecond,
			wantErr: true,
		},
	}

	for _, test := range tests {
		var listed []metav1.ListOptions
		sp := watchSpec{
			gvr: corev1.SchemeGroupVersion.WithResource("pods"),
			list: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				listed = append(listed, options)
				return test.list(ctx, options)
			},
		}
		timeout := time.Minute
		if test.timeout != 0 {
			timeout = test.timeout
		}

		// The check must only ever cancel its own child context, so the parent is checked after it returns.
		parent, parentCancel := context.WithCancel(t.Context())
		// bookmarkAhead runs on the pool so that a check that outlives its budget fails this test instead of hanging it.
		done := make(chan promises.Response[bool], 1)
		context.Pool(t.Context()).Submit(
			t.Context(),
			func() {
				ahead, err := bookmarkAhead(parent, sp, test.bookmark, timeout)
				done <- promises.Response[bool]{V: ahead, Err: err}
			},
		)
		waitCtx, waitCancel := context.WithTimeout(t.Context(), 5*time.Second)
		res, got := chans.Get(waitCtx, done)
		waitCancel()
		if !got.OK() {
			t.Errorf("TestBookmarkAhead(%s): bookmarkAhead did not return within 5s", test.name)
			parentCancel()
			continue
		}
		if parent.Err() != nil {
			t.Errorf("TestBookmarkAhead(%s): got parent ctx err == %v, want the parent untouched", test.name, parent.Err())
		}
		parentCancel()
		ahead, err := res.V, res.Err
		// Every list must be a consistent read of one object: no resourceVersion, so the server answers from etcd.
		for i, options := range listed {
			if diff := pretty.Compare(metav1.ListOptions{Limit: 1}, options); diff != "" {
				t.Errorf("TestBookmarkAhead(%s): list call %d options: -want/+got:\n%s", test.name, i, diff)
			}
		}
		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestBookmarkAhead(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestBookmarkAhead(%s): got err == %s, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}
		if ahead != test.wantAhead {
			t.Errorf("TestBookmarkAhead(%s): got ahead == %v, want %v", test.name, ahead, test.wantAhead)
		}
	}
}

func TestWatchEventsStoresBookmarks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// bookmarks is how many ordinary bookmark events with resourceVersion 700 the watcher sends.
		bookmarks int
		storeErr  error
		// initialEvents wraps the watcher in a WatchList recovery.
		initialEvents bool
		// completeRV, when set, is sent as the initial events completion bookmark after the ordinary bookmarks.
		completeRV string
		// listRV, when set, wraps the watcher in a list recovery of listObjects at that version.
		listRV      string
		listObjects []runtime.Object
		// expired makes the watcher end its stream with the APIServer's expired resourceVersion error event.
		expired    bool
		wantStored string
		wantCalls  int
		wantRV     string
		wantErr    bool
	}{
		{name: "Success: stores a bookmark", bookmarks: 1, wantStored: "700", wantRV: "700", wantCalls: 1},
		{name: "Success: skips storing a duplicate bookmark", bookmarks: 2, wantStored: "700", wantRV: "700", wantCalls: 1},
		{
			name:      "Success: retries storing a duplicate bookmark after a store error",
			bookmarks: 2,
			storeErr:  errors.New("bookmark ConfigMap update failed"),
			wantCalls: 2,
			wantRV:    "700",
		},
		{
			name:          "Success: incomplete recovery ignores ordinary bookmarks",
			bookmarks:     1,
			initialEvents: true,
		},
		{
			name:          "Success: recovery persists only the initial events completion bookmark",
			bookmarks:     1,
			initialEvents: true,
			completeRV:    "750",
			wantStored:    "750",
			wantRV:        "750",
			wantCalls:     1,
		},
		{
			name:   "Success: list recovery persists the list version once its snapshots are delivered",
			listRV: "600",
			listObjects: []runtime.Object{
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "a"}},
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", UID: "b"}},
			},
			wantStored: "600",
			wantRV:     "600",
			wantCalls:  1,
		},
		{
			name:    "Error: a stream rejecting a resumed resourceVersion returns the rejection",
			expired: true,
			wantErr: true,
		},
		{
			name:   "Error: list recovery returns the list version it persisted when the stream rejects it",
			listRV: "600",
			listObjects: []runtime.Object{
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "a"}},
			},
			expired:    true,
			wantStored: "600",
			wantRV:     "600",
			wantCalls:  1,
			wantErr:    true,
		},
	}

	for _, test := range tests {
		store := storefake.New(map[schema.GroupVersionResource]string{})
		store.SetStoreError(test.storeErr)
		// An unbuffered output means a snapshot is received only while watchEvents is blocked delivering it.
		r := &Reader{bookmarkStore: store, dataCh: make(chan data.Entry)}
		watcher := watch.NewFakeWithChanSize(test.bookmarks+2, false)
		for i := 0; i < test.bookmarks; i++ {
			watcher.Action(watch.Bookmark, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "700"}})
		}
		if test.completeRV != "" {
			watcher.Action(watch.Bookmark, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: test.completeRV,
				Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
			}})
		}
		if test.expired {
			watcher.Error(&apierrors.NewResourceExpired("too old resource version").ErrStatus)
		}
		watcher.Stop()

		var stream watch.Interface = watcher
		switch {
		case test.initialEvents:
			stream = newInitialEventsWatcher(watcher)
		case test.listRV != "":
			stream = newListRecoveryWatcher(watcher, test.listObjects, test.listRV)
		}

		done := make(chan promises.Response[string], 1)
		context.Pool(t.Context()).Submit(
			t.Context(),
			func() {
				rv, err := r.watchEvents(t.Context(), namespaceGVR, stream)
				done <- promises.Response[string]{V: rv, Err: err}
			},
		)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		for i, obj := range test.listObjects {
			// Until the last snapshot is received, watchEvents is still delivering, so nothing can be persisted.
			if got := store.StoreCalls().Len(); got != 0 {
				t.Errorf("TestWatchEventsStoresBookmarks(%s): got %d Store() calls before snapshot %d was delivered, want 0", test.name, got, i)
			}
			entry, res := chans.Get(ctx, r.dataCh)
			if !res.OK() {
				t.Errorf("TestWatchEventsStoresBookmarks(%s): got result %v for snapshot %d, want a snapshot", test.name, res, i)
				break
			}
			if diff := pretty.Compare(data.MustNewEntry(obj, data.STWatchList, data.CTSnapshot), entry); diff != "" {
				t.Errorf("TestWatchEventsStoresBookmarks(%s): snapshot %d -want/+got:\n%s", test.name, i, diff)
			}
		}
		res, got := chans.Get(ctx, done)
		cancel()
		if !got.OK() {
			t.Errorf("TestWatchEventsStoresBookmarks(%s): watchEvents did not return within 5s", test.name)
			continue
		}
		resourceVersion, err := res.V, res.Err
		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestWatchEventsStoresBookmarks(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestWatchEventsStoresBookmarks(%s): got err == %s, want err == nil", test.name, err)
			continue
		}
		// The resourceVersion and stored bookmark are checked on error rows too: handleWatcher clears the returned
		// resourceVersion when the stream rejected it, so what watchEvents returns and persisted is the contract.
		if resourceVersion != test.wantRV {
			t.Errorf("TestWatchEventsStoresBookmarks(%s): got resourceVersion == %q, want %q", test.name, resourceVersion, test.wantRV)
		}
		if got := store.Stored(namespaceGVR); got != test.wantStored {
			t.Errorf("TestWatchEventsStoresBookmarks(%s): got stored bookmark == %q, want %q", test.name, got, test.wantStored)
		}
		if got := store.StoreCalls().Len(); got != test.wantCalls {
			t.Errorf("TestWatchEventsStoresBookmarks(%s): got %d Store() calls, want %d", test.name, got, test.wantCalls)
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

	doneCtx, cancel := context.WithCancel(t.Context())
	cancel()

	closedIn := make(chan watch.Event)
	close(closedIn)

	// stalledCtx is cancelled while watchEvent is blocked sending to a full output channel, which is what Close() does
	// to a watcher whose consumer has stalled.
	stalledCtx, stalledCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stalledCancel()

	tests := []struct {
		name      string
		ctx       context.Context
		ch        chan watch.Event
		wantRV    string
		event     watch.Event
		wantEntry data.Entry
		wantErr   bool
		// wantStale is set when the error must report the stream rejecting its resourceVersion as stale.
		wantStale   bool
		wantStopper bool
		// fullOut makes the output channel full before watchEvent is called, so sending an entry blocks.
		fullOut bool
		// initialEvents is set when the watcher is still streaming WatchList recovery snapshots.
		initialEvents     bool
		wantInitialEvents bool
	}{
		{
			name:        "Error: a done context stops the watcher",
			ctx:         doneCtx,
			ch:          make(chan watch.Event),
			wantErr:     true,
			wantStopper: true,
		},
		{
			name:        "Error: the input channel is closed",
			ctx:         t.Context(),
			ch:          closedIn,
			wantErr:     true,
			wantStopper: true,
		},
		{
			name: "Success: a bookmark event returns its resourceVersion",
			ctx:  t.Context(),
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
			name: "Success: an event is sent to the output channel",
			ctx:  t.Context(),
			ch:   make(chan watch.Event, 1),
			event: watch.Event{
				Type:   watch.Added,
				Object: &corev1.Pod{},
			},
			wantEntry: data.MustNewEntry(&corev1.Pod{}, data.STWatchList, data.CTAdd),
		},
		{
			name: "Success: an added event during initial events is a snapshot",
			ctx:  t.Context(),
			ch:   make(chan watch.Event, 1),
			event: watch.Event{
				Type:   watch.Added,
				Object: &corev1.Pod{},
			},
			initialEvents:     true,
			wantInitialEvents: true,
			wantEntry:         data.MustNewEntry(&corev1.Pod{}, data.STWatchList, data.CTSnapshot),
		},
		{
			name: "Success: an ordinary bookmark during initial events is ignored",
			ctx:  t.Context(),
			ch:   make(chan watch.Event, 1),
			event: watch.Event{
				Type:   watch.Bookmark,
				Object: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}},
			},
			initialEvents:     true,
			wantInitialEvents: true,
		},
		{
			name: "Success: the initial events completion bookmark returns its resourceVersion and ends initial events",
			ctx:  t.Context(),
			ch:   make(chan watch.Event, 1),
			event: watch.Event{
				Type: watch.Bookmark,
				Object: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: "2",
					Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
				}},
			},
			initialEvents: true,
			wantRV:        "2",
		},
		{
			name: "Error: context cancelled while the output channel is full stops the watcher",
			ctx:  stalledCtx,
			ch:   make(chan watch.Event, 1),
			event: watch.Event{
				Type:   watch.Added,
				Object: &corev1.Pod{},
			},
			fullOut:     true,
			wantErr:     true,
			wantStopper: true,
		},
		{
			name: "Error: an expired resourceVersion error event stops the watcher",
			ctx:  t.Context(),
			ch:   make(chan watch.Event, 1),
			event: watch.Event{
				Type:   watch.Error,
				Object: &apierrors.NewResourceExpired("too old resource version: 100").ErrStatus,
			},
			wantErr:     true,
			wantStale:   true,
			wantStopper: true,
		},
		{
			name: "Error: a gone resourceVersion error event stops the watcher",
			ctx:  t.Context(),
			ch:   make(chan watch.Event, 1),
			event: watch.Event{
				Type:   watch.Error,
				Object: &apierrors.NewGone("resourceVersion is gone").ErrStatus,
			},
			wantErr:     true,
			wantStale:   true,
			wantStopper: true,
		},
		{
			name: "Success: any other error event is logged and the watch continues",
			ctx:  t.Context(),
			ch:   make(chan watch.Event, 1),
			event: watch.Event{
				Type:   watch.Error,
				Object: &apierrors.NewInternalError(errors.New("etcd unavailable")).ErrStatus,
			},
		},
	}

	for _, test := range tests {
		r := &Reader{dataCh: make(chan data.Entry, 1)}
		if test.fullOut {
			r.dataCh <- data.Entry{}
		}
		if test.event != (watch.Event{}) {
			test.ch <- test.event
		}

		stopped := false
		stopper := newStopper(&stopped)
		initialEvents := test.initialEvents

		// watchEvent runs on the pool so that a watchEvent that never returns fails this test instead of hanging it.
		done := make(chan promises.Response[string], 1)
		context.Pool(t.Context()).Submit(
			t.Context(),
			func() {
				rv, err := r.watchEvent(test.ctx, test.ch, stopper, &initialEvents)
				done <- promises.Response[string]{V: rv, Err: err}
			},
		)
		guardCtx, guardCancel := context.WithTimeout(t.Context(), 5*time.Second)
		res, got := chans.Get(guardCtx, done)
		guardCancel()
		if !got.OK() {
			t.Errorf("TestWatchEvent(%s): watchEvent did not return within 5s", test.name)
			continue
		}
		gotRv, err := res.V, res.Err
		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestWatchEvent(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("TestWatchEvent(%s): got err == %v, want err == nil", test.name, err)
			continue
		}
		// handleWatcher branches on staleResourceVersion(err) to clear the bookmark and start a recovery instead of
		// reconnecting at the same resourceVersion, so the error's type is the contract, not just that it failed.
		if gotStale := staleResourceVersion(err); gotStale != test.wantStale {
			t.Errorf("TestWatchEvent(%s): got staleResourceVersion(err) == %v, want %v: %v", test.name, gotStale, test.wantStale, err)
		}
		if stopped != test.wantStopper {
			t.Errorf("TestWatchEvent(%s): got stopped == %v, want stopped == %v", test.name, stopped, test.wantStopper)
		}
		if gotRv != test.wantRV {
			t.Errorf("TestWatchEvent(%s): got rv == %s, want rv == %s", test.name, gotRv, test.wantRV)
		}
		if initialEvents != test.wantInitialEvents {
			t.Errorf("TestWatchEvent(%s): got initialEvents == %v, want %v", test.name, initialEvents, test.wantInitialEvents)
		}
		if err != nil {
			continue
		}
		if !reflect.ValueOf(test.wantEntry).IsZero() {
			if diff := pretty.Compare(test.wantEntry, <-r.dataCh); diff != "" {
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
			name:           "Success: relist disabled",
			relistInterval: -1,
			wantCalled:     true, // testHandleClientSwitch is always called for testing
		},
		{
			name:           "Success: relist enabled",
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

		r.relistTask(t.Context())

		if handleClientSwitchCalled != test.wantCalled {
			t.Errorf("TestRelistTask(%s): handleClientSwitch called = %v, want %v", test.name, handleClientSwitchCalled, test.wantCalled)
		}
	}
}

// TestPerformRelist tests the performRelist functionality
func TestPerformRelist(t *testing.T) {
	t.Parallel()

	newPod := func(uid string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: uid, Namespace: "default", UID: k8stypes.UID(uid)}}
	}
	clientset := fake.NewSimpleClientset(newPod("uid-1"), newPod("uid-2"))
	newRelister := func() lister {
		r, err := relist.New(clientset)
		if err != nil {
			t.Fatalf("TestPerformRelist: relist.New() got err == %s, want err == nil", err)
		}
		return r
	}

	// respond returns a closed channel holding resps, which is how relist.Relister ends a list.
	respond := func(resps ...promises.Response[data.Entry]) chan promises.Response[data.Entry] {
		ch := make(chan promises.Response[data.Entry], len(resps))
		for _, resp := range resps {
			ch <- resp
		}
		close(ch)
		return ch
	}
	snapshot := func(uid string) promises.Response[data.Entry] {
		return promises.Response[data.Entry]{V: data.MustNewEntry(newPod(uid), data.STWatchList, data.CTSnapshot)}
	}
	// The first list fails partway through after sending uid-1. The retry must happen and must not resend uid-1.
	failCalls := 0
	failOnce := &fakeRelister{
		listFunc: func(ctx context.Context, rt types.Retrieve) (chan promises.Response[data.Entry], error) {
			failCalls++
			if failCalls == 1 {
				return respond(snapshot("uid-1"), promises.Response[data.Entry]{Err: errors.New("list error")}), nil
			}
			return respond(snapshot("uid-1"), snapshot("uid-2")), nil
		},
	}

	tests := []struct {
		name      string
		retrieves types.Retrieve
		relister  lister
		// cancelled cancels the context before performRelist is called.
		cancelled bool
		wantErr   bool
		// wantUIDs are the UIDs sent, sorted.
		wantUIDs []k8stypes.UID
		// calls, when set, reports how many times List was called and must equal wantCalls.
		calls     func() int
		wantCalls int
	}{
		{
			name:      "Success: relists every pod as a snapshot",
			retrieves: types.RTPod,
			relister:  newRelister(),
			wantUIDs:  []k8stypes.UID{"uid-1", "uid-2"},
		},
		{
			name:      "Error: the context is already cancelled",
			retrieves: types.RTPod,
			relister:  newRelister(),
			cancelled: true,
			wantErr:   true,
		},
		{
			name:      "Success: a type with no objects adds nothing",
			retrieves: types.RTPod | types.RTNamespace,
			relister:  newRelister(),
			wantUIDs:  []k8stypes.UID{"uid-1", "uid-2"},
		},
		{
			name:      "Success: a list that fails partway is retried without resending what was already sent",
			retrieves: types.RTPod,
			relister:  failOnce,
			wantUIDs:  []k8stypes.UID{"uid-1", "uid-2"},
			calls:     func() int { return failCalls },
			wantCalls: 2,
		},
	}

	for _, test := range tests {
		ctx := t.Context()
		if test.cancelled {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		}

		// The channel holds every entry, so performRelist never blocks sending and can run here.
		ch := make(chan data.Entry, 10)
		r := &Reader{
			retrieveTypes:  test.retrieves,
			relister:       test.relister,
			dataCh:         ch,
			relistInterval: 1 * time.Hour,
		}

		err := r.performRelist(ctx)
		close(ch)
		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestPerformRelist(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("TestPerformRelist(%s): got err == %s, want err == nil", test.name, err)
			continue
		}

		var gotUIDs []k8stypes.UID
		for entry := range ch {
			if entry.ChangeType() != data.CTSnapshot {
				t.Errorf("TestPerformRelist(%s): entry %s ChangeType() = %v, want %v", test.name, entry.UID(), entry.ChangeType(), data.CTSnapshot)
			}
			gotUIDs = append(gotUIDs, entry.UID())
		}
		slices.Sort(gotUIDs)
		if diff := pretty.Compare(test.wantUIDs, gotUIDs); diff != "" {
			t.Errorf("TestPerformRelist(%s): sent UIDs: -want/+got:\n%s", test.name, diff)
		}
		if test.calls != nil && test.calls() != test.wantCalls {
			t.Errorf("TestPerformRelist(%s): got %d calls to List(), want %d", test.name, test.calls(), test.wantCalls)
		}
	}
}

func TestHandleWatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// watchEventErrors are returned by successive watchEvents calls; after them watchEvents watches until ctx is done.
		watchEventErrors []error
		// spawnErrors are returned by successive reconnects; after them a reconnect succeeds.
		spawnErrors []error
		// cancelWhileRetrying keeps every reconnect after spawnErrors failing and cancels ctx once they are all used,
		// so handleWatcher is still retrying when the Reader stops, as watchCtx has no deadline in production.
		cancelWhileRetrying bool
		wantErr             bool
	}{
		{
			name:             "Success: handles watcher until context cancelled",
			watchEventErrors: []error{nil, nil},
		},
		{
			name:             "Success: recovers from watchEvents error",
			watchEventErrors: []error{errors.New("watch error"), nil},
			spawnErrors:      []error{nil},
		},
		{
			name:                "Success: retries failed reconnects until context cancelled",
			watchEventErrors:    []error{errors.New("watch error")},
			spawnErrors:         []error{errors.New("spawn error 1"), errors.New("spawn error 2"), errors.New("spawn error 3")},
			cancelWhileRetrying: true,
		},
		{
			name:             "Error: forbidden reconnect ends the watcher",
			watchEventErrors: []error{errors.New("watch error")},
			spawnErrors:      []error{apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", errors.New("rbac"))},
			wantErr:          true,
		},
	}

	for _, test := range tests {
		ctx, cancel := context.WithCancel(t.Context())

		watchEventIndex := 0
		spawnIndex := 0
		// progress receives once handleWatcher reaches the point a success row cancels at: watching until ctx is
		// done, or still retrying failed reconnects.
		progress := make(chan struct{}, 1)

		r := &Reader{
			spawnCh:           make(chan promises.Value[spawnWatcher, watch.Interface]),
			watcherSpawnDelay: 1 * time.Millisecond,
			fakeWatchEvents: func(ctx context.Context, watcher watch.Interface) (string, error) {
				if watchEventIndex < len(test.watchEventErrors) {
					err := test.watchEventErrors[watchEventIndex]
					watchEventIndex++
					if err != nil {
						return "", err
					}
				}
				chans.TryPut(progress, struct{}{})
				<-ctx.Done()
				return "", nil
			},
		}

		context.Pool(t.Context()).Submit(t.Context(), func() { r.connectWatcher(ctx, r.spawnCh) })

		sp := func(options metav1.ListOptions) (watch.Interface, error) {
			if spawnIndex < len(test.spawnErrors) {
				err := test.spawnErrors[spawnIndex]
				spawnIndex++
				if err != nil {
					return nil, err
				}
			}
			if test.cancelWhileRetrying {
				chans.TryPut(progress, struct{}{})
				return nil, errors.New("spawn still failing")
			}
			return &fakeWatcher{}, nil
		}

		// handleWatcher runs on the pool so that one that never returns fails this test instead of hanging it.
		done := make(chan error, 1)
		context.Pool(t.Context()).Submit(
			t.Context(),
			func() {
				done <- r.handleWatcher(ctx, types.RTNamespace, namespaceGVR, "", &fakeWatcher{}, watchSpec{gvr: namespaceGVR, spawn: sp})
			},
		)

		waitCtx, waitCancel := context.WithTimeout(t.Context(), 5*time.Second)
		if !test.wantErr {
			if _, res := chans.Get(waitCtx, progress); !res.OK() {
				t.Errorf("TestHandleWatcher(%s): handleWatcher did not reach the point to cancel at within 5s", test.name)
				waitCancel()
				cancel()
				continue
			}
			cancel()
		}
		err, res := chans.Get(waitCtx, done)
		waitCancel()
		cancel()
		if !res.OK() {
			t.Errorf("TestHandleWatcher(%s): handleWatcher did not return within 5s", test.name)
			continue
		}
		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestHandleWatcher(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestHandleWatcher(%s): got err == %s, want err == nil", test.name, err)
			continue
		}
	}
}

func TestGetWatcher(t *testing.T) {
	t.Parallel()

	spawnErr := errors.New("connection failed")

	tests := []struct {
		name         string
		cancelBefore bool
		spawn        spawnWatcher
		setupFn      func(r *Reader, ctx context.Context)
		wantErr      bool
		// wantWrapped, when set, is the spawn error the returned error must wrap.
		wantWrapped error
	}{
		{
			name: "Success: watcher created successfully",
			spawn: func(options metav1.ListOptions) (watch.Interface, error) {
				return &fakeWatcher{}, nil
			},
			setupFn: func(r *Reader, ctx context.Context) {
				context.Pool(t.Context()).Submit(t.Context(), func() { r.connectWatcher(ctx, r.spawnCh) })
			},
			wantErr: false,
		},
		{
			name: "Error: spawn function returns error",
			spawn: func(options metav1.ListOptions) (watch.Interface, error) {
				return nil, spawnErr
			},
			setupFn: func(r *Reader, ctx context.Context) {
				context.Pool(t.Context()).Submit(t.Context(), func() { r.connectWatcher(ctx, r.spawnCh) })
			},
			wantErr:     true,
			wantWrapped: spawnErr,
		},
		{
			name:         "Error: context cancelled before sending",
			cancelBefore: true,
			spawn: func(options metav1.ListOptions) (watch.Interface, error) {
				return &fakeWatcher{}, nil
			},
			setupFn: func(r *Reader, ctx context.Context) {
				// Don't start connectWatcher, and use cancelled context
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		r := &Reader{
			spawnCh:           make(chan promises.Value[spawnWatcher, watch.Interface]),
			watcherSpawnDelay: 1 * time.Millisecond, // Short for testing
		}

		if test.cancelBefore {
			cancel()
		} else {
			test.setupFn(r, ctx)
			time.Sleep(10 * time.Millisecond) // Give goroutine time to start
		}

		w, err := r.getWatcher(ctx, types.RTNamespace, test.spawn)

		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestGetWatcher(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestGetWatcher(%s): got err == %v, want err == nil", test.name, err)
			continue
		case err != nil:
			// handleWatcher's retry classifies the spawn error through this wrap (apierrors.IsForbidden,
			// rejectedResourceVersion, invalidWatchRequest), and watch() and getRecoveryWatcher check
			// errors.Is(err, errListRecovery) on it, so the spawn error must stay reachable from the returned error.
			if test.wantWrapped != nil && !errors.Is(err, test.wantWrapped) {
				t.Errorf("TestGetWatcher(%s): got err == %v, want it to wrap %v", test.name, err, test.wantWrapped)
			}
			continue
		}

		if w == nil {
			t.Errorf("TestGetWatcher(%s): got watcher == nil, want watcher != nil", test.name)
		}
	}
}

func TestConnectWatcherThrottling(t *testing.T) {
	// Not parallel: it asserts the delay between watcher creations, which other tests competing for CPU would skew.

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	r := &Reader{
		spawnCh:           make(chan promises.Value[spawnWatcher, watch.Interface]),
		watcherSpawnDelay: 50 * time.Millisecond, // Short delay for testing
	}

	// Track timing of watcher creations
	var creationTimes []time.Time
	watcherCount := 3

	// Start the connectWatcher goroutine
	context.Pool(t.Context()).Submit(t.Context(), func() { r.connectWatcher(ctx, r.spawnCh) })

	// Send multiple spawn requests
	for range watcherCount {
		promise := spawnReqMaker.New(
			ctx,
			// This is a fake spawnWatcher that records creation time vs actually doing anything.
			func(options metav1.ListOptions) (watch.Interface, error) {
				creationTimes = append(creationTimes, time.Now())
				return &fakeWatcher{}, nil
			},
		)

		sendCtx, sendCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		sent := chans.Put(sendCtx, r.spawnCh, promise)
		sendCancel()
		if !sent {
			t.Fatalf("TestConnectWatcherThrottling: timeout sending to spawn channel")
		}

		resp, err := promise.Get(ctx)
		if err != nil {
			t.Errorf("TestConnectWatcherThrottling: promise.Get() error = %v", err)
		}
		if resp.Err != nil {
			t.Errorf("TestConnectWatcherThrottling: spawn error = %v", resp.Err)
		}
	}

	// Verify throttling occurred - watchers should be created with delays
	if len(creationTimes) != watcherCount {
		t.Errorf("TestConnectWatcherThrottling: got %d creation times, want %d", len(creationTimes), watcherCount)
	}

	// Check delays between creations (should be at least watcherSpawnDelay)
	for i := 1; i < len(creationTimes); i++ {
		delay := creationTimes[i].Sub(creationTimes[i-1])
		// Allow some tolerance for timing
		minDelay := r.watcherSpawnDelay - 10*time.Millisecond
		if delay < minDelay {
			t.Errorf("TestConnectWatcherThrottling: delay between watcher %d and %d was %v, expected at least %v",
				i-1, i, delay, minDelay)
		}
	}
}

// fakeRelister is a mock implementation of the Relister interface
type fakeRelister struct {
	listFunc func(ctx context.Context, rt types.Retrieve) (chan promises.Response[data.Entry], error)
}

func (m *fakeRelister) List(ctx context.Context, rt types.Retrieve) (chan promises.Response[data.Entry], error) {
	if m.listFunc != nil {
		return m.listFunc(ctx, rt)
	}
	return nil, errors.New("not implemented")
}

func TestMinorInt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want int
	}{
		{name: "Success: plain number", in: "30", want: 30},
		{name: "Success: number with plus suffix", in: "30+", want: 30},
		{name: "Success: number with dot suffix", in: "30.1", want: 30},
		{name: "Success: empty string", in: "", want: 0},
		{name: "Success: non-numeric string", in: "abc", want: 0},
		{name: "Success: zero", in: "0", want: 0},
	}

	for _, test := range tests {
		got := convertToDecimal(test.in)
		if got != test.want {
			t.Errorf("TestMinorInt(%s): got %d, want %d", test.name, got, test.want)
		}
	}
}

func TestMajorMinor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		major string
		minor string
		want  [2]int
	}{
		{name: "Success: version 1.30", major: "1", minor: "30", want: [2]int{1, 30}},
		{name: "Success: version with plus suffix", major: "1", minor: "30+", want: [2]int{1, 30}},
	}

	for _, test := range tests {
		clientset := fake.NewSimpleClientset()
		fakeDisc := clientset.Discovery().(*fakediscovery.FakeDiscovery)
		fakeDisc.FakedServerVersion = &version.Info{Major: test.major, Minor: test.minor}

		got, err := majorMinor(clientset)
		if err != nil {
			t.Errorf("TestMajorMinor(%s): got err == %s, want err == nil", test.name, err)
			continue
		}
		if got != test.want {
			t.Errorf("TestMajorMinor(%s): got %v, want %v", test.name, got, test.want)
		}
	}
}

func TestBookmarkingVersionThreshold(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		minor           string
		opts            []Option
		wantBookmarking bool
	}{
		{name: "Success: bookmarking enabled for 1.29", minor: "29", wantBookmarking: true},
		{name: "Success: bookmarking enabled for 1.30", minor: "30", wantBookmarking: true},
		{name: "Success: bookmarking enabled for 1.31", minor: "31", wantBookmarking: true},
		{
			name:            "Success: bookmark store falls back on 1.28",
			minor:           "28",
			opts:            []Option{WithBookmarkStore(storefake.New(nil))},
			wantBookmarking: false,
		},
		{name: "Success: bookmarking disabled for 1.14", minor: "14", wantBookmarking: false},
	}

	for _, test := range tests {
		clientset := fake.NewSimpleClientset()
		fakeDisc := clientset.Discovery().(*fakediscovery.FakeDiscovery)
		fakeDisc.FakedServerVersion = &version.Info{Major: "1", Minor: test.minor}

		r, err := New(t.Context(), clientset, types.RTPod, test.opts...)
		if err != nil {
			t.Errorf("TestBookmarkingVersionThreshold(%s): got err == %s, want err == nil", test.name, err)
			continue
		}
		if r.bookmarking != test.wantBookmarking {
			t.Errorf("TestBookmarkingVersionThreshold(%s): got bookmarking == %v, want bookmarking == %v", test.name, r.bookmarking, test.wantBookmarking)
		}
	}
}

func TestConnectWatcherBookmarkOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		bookmarking bool
		// resourceVersion is the bookmark the watch resumes from, applied with withResourceVersion as
		// Reader.watch and Reader.handleWatcher do.
		resourceVersion          string
		wantWatch                bool
		wantAllowWatchBookmarks  bool
		wantResourceVersion      string
		wantResourceVersionMatch metav1.ResourceVersionMatch
		wantSendInitialEvents    *bool
	}{
		{
			name:                     "Success: bookmarking disabled sends only Watch true",
			bookmarking:              false,
			wantWatch:                true,
			wantAllowWatchBookmarks:  false,
			wantResourceVersion:      "",
			wantResourceVersionMatch: "",
			wantSendInitialEvents:    nil,
		},
		{
			name:                     "Success: bookmarking enabled without a bookmark asks for initial events so existing objects are sent",
			bookmarking:              true,
			wantWatch:                true,
			wantAllowWatchBookmarks:  true,
			wantResourceVersion:      "",
			wantResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
			wantSendInitialEvents:    ptr.To(true),
		},
		{
			name:                     "Success: bookmarking enabled resuming from a bookmark does not replay initial events",
			bookmarking:              true,
			resourceVersion:          "100",
			wantWatch:                true,
			wantAllowWatchBookmarks:  true,
			wantResourceVersion:      "100",
			wantResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
			wantSendInitialEvents:    ptr.To(false),
		},
	}

	for _, test := range tests {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var capturedOptions metav1.ListOptions
		r := &Reader{
			spawnCh:           make(chan promises.Value[spawnWatcher, watch.Interface]),
			watcherSpawnDelay: 1 * time.Millisecond,
			bookmarking:       test.bookmarking,
		}

		context.Pool(t.Context()).Submit(t.Context(), func() { r.connectWatcher(ctx, r.spawnCh) })

		sp := func(options metav1.ListOptions) (watch.Interface, error) {
			capturedOptions = options
			return &fakeWatcher{}, nil
		}
		promise := spawnReqMaker.New(ctx, withResourceVersion(sp, test.resourceVersion))

		r.spawnCh <- promise
		_, err := promise.Get(ctx)
		if err != nil {
			t.Errorf("TestConnectWatcherBookmarkOptions(%s): got err == %s, want err == nil", test.name, err)
			continue
		}

		if capturedOptions.Watch != test.wantWatch {
			t.Errorf("TestConnectWatcherBookmarkOptions(%s): got Watch == %v, want Watch == %v", test.name, capturedOptions.Watch, test.wantWatch)
		}
		if capturedOptions.AllowWatchBookmarks != test.wantAllowWatchBookmarks {
			t.Errorf("TestConnectWatcherBookmarkOptions(%s): got AllowWatchBookmarks == %v, want AllowWatchBookmarks == %v", test.name, capturedOptions.AllowWatchBookmarks, test.wantAllowWatchBookmarks)
		}
		if capturedOptions.ResourceVersion != test.wantResourceVersion {
			t.Errorf("TestConnectWatcherBookmarkOptions(%s): got ResourceVersion == %q, want ResourceVersion == %q", test.name, capturedOptions.ResourceVersion, test.wantResourceVersion)
		}
		if capturedOptions.ResourceVersionMatch != test.wantResourceVersionMatch {
			t.Errorf("TestConnectWatcherBookmarkOptions(%s): got ResourceVersionMatch == %q, want ResourceVersionMatch == %q", test.name, capturedOptions.ResourceVersionMatch, test.wantResourceVersionMatch)
		}
		if diff := pretty.Compare(test.wantSendInitialEvents, capturedOptions.SendInitialEvents); diff != "" {
			t.Errorf("TestConnectWatcherBookmarkOptions(%s): SendInitialEvents: -want/+got:\n%s", test.name, diff)
		}

		cancel()
	}
}

func TestHandleWatcherBookmarkReconnection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		bookmarking bool
		bookmarkRV  string
		// spawnErr is returned by the first spawn, as an APIServer that refuses the watch request.
		spawnErr error
		// streamErr is returned by the first watchEvents, as an APIServer that rejects the resourceVersion inside the
		// stream after accepting the watch request.
		streamErr error
		// resumed starts handleWatcher at bookmarkRV, as a watch resumed from the stored bookmark, so the first
		// watchEvents reports no version of its own: a rejected resumed stream carries only the error. Otherwise
		// handleWatcher starts without a version and the first watchEvents reports bookmarkRV, as a list recovery
		// reports the list's version.
		resumed bool
		// deleteErr makes clearing the bookmark from the store fail.
		deleteErr               error
		replacementRV           string
		wantRVOnFirstReconnect  string
		wantRVOnSecondReconnect string
		wantDelete              bool
		wantErr                 bool
		// wantRecovery is set when a rejected bookmark must restart the ordered initial-events stream.
		wantRecovery bool
	}{
		{
			name:                    "Success: reconnection uses bookmarked resource version",
			bookmarking:             true,
			bookmarkRV:              "100",
			wantRVOnFirstReconnect:  "100",
			wantRVOnSecondReconnect: "",
		},
		{
			name:                    "Success: expired resource version resets bookmark",
			bookmarking:             true,
			bookmarkRV:              "100",
			spawnErr:                apierrors.NewResourceExpired("the resourceVersion is too old"),
			wantRVOnFirstReconnect:  "100",
			wantRVOnSecondReconnect: "",
			wantDelete:              true,
			wantRecovery:            true,
		},
		{
			name:                    "Success: gone resource version resets bookmark",
			bookmarking:             true,
			bookmarkRV:              "100",
			spawnErr:                apierrors.NewGone("the resourceVersion is gone"),
			wantRVOnFirstReconnect:  "100",
			wantRVOnSecondReconnect: "",
			wantDelete:              true,
			wantRecovery:            true,
		},
		{
			name:                    "Success: malformed resource version resets bookmark",
			bookmarking:             true,
			bookmarkRV:              "malformed",
			spawnErr:                apierrors.NewBadRequest("invalid resourceVersion"),
			wantRVOnFirstReconnect:  "malformed",
			wantRVOnSecondReconnect: "",
			wantDelete:              true,
			wantRecovery:            true,
		},
		{
			name:                    "Success: transient failure preserves bookmark",
			bookmarking:             true,
			bookmarkRV:              "100",
			spawnErr:                errors.New("temporary API outage"),
			wantRVOnFirstReconnect:  "100",
			wantRVOnSecondReconnect: "100",
		},
		{
			name:                    "Success: stale watcher leaves another reader's replacement bookmark in place and recovers instead of resuming from it",
			bookmarking:             true,
			bookmarkRV:              "100",
			spawnErr:                apierrors.NewResourceExpired("the resourceVersion is too old"),
			replacementRV:           "500",
			wantRVOnFirstReconnect:  "100",
			wantRVOnSecondReconnect: "",
			wantRecovery:            true,
		},
		{
			name:                    "Success: no bookmarking does not set resource version",
			bookmarking:             false,
			bookmarkRV:              "100",
			wantRVOnFirstReconnect:  "",
			wantRVOnSecondReconnect: "",
		},
		{
			name:                   "Success: resumed bookmark rejected inside the stream resets bookmark",
			bookmarking:            true,
			bookmarkRV:             "100",
			resumed:                true,
			streamErr:              apierrors.NewResourceExpired("too old resource version: 100"),
			wantRVOnFirstReconnect: "",
			wantDelete:             true,
			wantRecovery:           true,
		},
		{
			name:                   "Success: list recovery version rejected inside the stream resets bookmark",
			bookmarking:            true,
			bookmarkRV:             "100",
			streamErr:              apierrors.NewResourceExpired("too old resource version: 100"),
			wantRVOnFirstReconnect: "",
			wantDelete:             true,
			wantRecovery:           true,
		},
		{
			name:                   "Success: another stream error preserves bookmark",
			bookmarking:            true,
			bookmarkRV:             "100",
			streamErr:              errors.New("error converting recovery snapshot"),
			wantRVOnFirstReconnect: "100",
		},
		{
			name:                   "Success: a failed bookmark clear after a stream rejection still recovers",
			bookmarking:            true,
			bookmarkRV:             "100",
			streamErr:              apierrors.NewResourceExpired("too old resource version: 100"),
			deleteErr:              errors.New("bookmark ConfigMap update failed"),
			wantRVOnFirstReconnect: "",
			wantRecovery:           true,
		},
		{
			name:        "Success: a permanent bookmark clear failure after a rejected reconnect still recovers",
			bookmarking: true,
			bookmarkRV:  "100",
			spawnErr:    apierrors.NewResourceExpired("the resourceVersion is too old"),
			// The configmap store classifies a forbidden ConfigMap update as permanent, which must not stop the
			// reconnect retry.
			deleteErr:               apierr.Permanent(apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "bookmarks", errors.New("rbac"))),
			wantRVOnFirstReconnect:  "100",
			wantRVOnSecondReconnect: "",
			wantRecovery:            true,
		},
		{
			name:                   "Error: forbidden reconnect is permanent",
			bookmarking:            true,
			bookmarkRV:             "100",
			spawnErr:               apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", errors.New("forbidden")),
			wantRVOnFirstReconnect: "100",
			wantErr:                true,
		},
	}

	for _, test := range tests {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		watchCallCount := 0
		var capturedOptions []metav1.ListOptions
		spawnCallCount := 0
		store := storefake.New(map[schema.GroupVersionResource]string{namespaceGVR: test.bookmarkRV})
		store.SetDeleteError(test.deleteErr)
		relisted := make(chan types.Retrieve, 1)
		// parked is closed once handleWatcher has finished reconnecting and is watching until ctx is cancelled.
		parked := make(chan struct{})

		r := &Reader{
			spawnCh:           make(chan promises.Value[spawnWatcher, watch.Interface]),
			watcherSpawnDelay: 1 * time.Millisecond,
			bookmarking:       test.bookmarking,
			bookmarkStore:     store,
			relister:          newRecordingRelister(relisted),
			dataCh:            make(chan data.Entry, 1),
			fakeWatchEvents: func(ctx context.Context, watcher watch.Interface) (string, error) {
				watchCallCount++
				switch watchCallCount {
				case 1:
					if test.resumed {
						return "", test.streamErr
					}
					return test.bookmarkRV, test.streamErr
				case 2:
					return "", nil
				default:
					if watchCallCount == 3 {
						close(parked)
					}
					<-ctx.Done()
					return "", nil
				}
			},
		}

		context.Pool(t.Context()).Submit(t.Context(), func() { r.connectWatcher(ctx, r.spawnCh) })

		sp := func(options metav1.ListOptions) (watch.Interface, error) {
			spawnCallCount++
			capturedOptions = append(capturedOptions, options)
			if test.spawnErr != nil && spawnCallCount == 1 {
				if test.replacementRV != "" {
					if err := store.Store(ctx, namespaceGVR, test.replacementRV); err != nil {
						t.Fatalf("Store() got err %v, want nil", err)
					}
				}
				return nil, test.spawnErr
			}
			return &fakeWatcher{}, nil
		}

		done := make(chan error, 1)
		context.Pool(t.Context()).Submit(
			t.Context(),
			func() {
				startRV := ""
				if test.resumed {
					startRV = test.bookmarkRV
				}
				done <- r.handleWatcher(ctx, types.RTNamespace, namespaceGVR, startRV, &fakeWatcher{}, watchSpec{gvr: namespaceGVR, spawn: sp})
			},
		)

		// A permanent error ends handleWatcher before it parks. Both waits are bounded so a handleWatcher that keeps
		// reconnecting fails this test instead of hanging it.
		waitCtx, waitCancel := context.WithTimeout(t.Context(), 5*time.Second)
		var err error
		var res chans.Result
		select {
		case <-parked:
			cancel()
			err, res = chans.Get(waitCtx, done)
		case err = <-done:
			res = chans.ResultOK
		case <-waitCtx.Done():
			res = chans.ResultCtxDone
		}
		waitCancel()
		cancel()
		_ = r.waitWatchers.Wait(t.Context())
		if !res.OK() {
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): handleWatcher did not return within 5s", test.name)
			continue
		}
		if _, gotRelist, _ := chans.TryGet(relisted); gotRelist {
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): got an independent relist, want ordered recovery only", test.name)
		}
		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): got err == %s, want err == nil", test.name, err)
			continue
		}

		if len(capturedOptions) < 1 {
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): expected at least 1 spawn call, got %d", test.name, len(capturedOptions))
			continue
		}

		if capturedOptions[0].ResourceVersion != test.wantRVOnFirstReconnect {
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): first reconnect got ResourceVersion == %q, want %q", test.name, capturedOptions[0].ResourceVersion, test.wantRVOnFirstReconnect)
		}

		if test.spawnErr != nil && len(capturedOptions) >= 2 && capturedOptions[1].ResourceVersion != test.wantRVOnSecondReconnect {
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): second reconnect got ResourceVersion == %q, want %q", test.name, capturedOptions[1].ResourceVersion, test.wantRVOnSecondReconnect)
		}
		// Every spawn after a rejected bookmark must be a recovery until its completion bookmark arrives, and the fake
		// never sends one, so the last spawn shows whether the rejection started a recovery.
		if test.wantRecovery {
			if diff := pretty.Compare(ptr.To(true), capturedOptions[len(capturedOptions)-1].SendInitialEvents); diff != "" {
				t.Errorf("TestHandleWatcherBookmarkReconnection(%s): recovery reconnect SendInitialEvents: -want/+got:\n%s", test.name, diff)
			}
		}
		if gotDelete := store.Deletes().Len() > 0; gotDelete != test.wantDelete {
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): got bookmark delete == %v, want %v", test.name, gotDelete, test.wantDelete)
		}
	}
}
