package watchlist

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store/configmap"
	storefake "github.com/Azure/tattler/readers/apiserver/watchlist/bookmarks/store/fake"
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
		watchers := test.create(&Reader{}, t.Context())
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
		// noWatchList makes the server reject WatchList options, as one without the WatchList feature gate does.
		noWatchList bool
		wantRVs     []string
		wantDelete  bool
		wantErr     bool
		// wantRelist is set when a rejected bookmark must be recovered with a snapshot relist. The last watch must
		// then be created with wantRecoverySendInitialEvents.
		wantRelist                    bool
		wantRecoverySendInitialEvents *bool
	}{
		{
			name:     "Success: uses stored bookmark on startup",
			storedRV: "500",
			wantRVs:  []string{"500"},
		},
		{
			name:                          "Success: clears expired bookmark, watches from now and relists",
			storedRV:                      "500",
			watchErr:                      apierrors.NewResourceExpired("the resourceVersion is too old"),
			wantRVs:                       []string{"500", ""},
			wantDelete:                    true,
			wantRelist:                    true,
			wantRecoverySendInitialEvents: ptr.To(false),
		},
		{
			name:                          "Success: clears malformed bookmark, watches from now and relists",
			storedRV:                      "malformed",
			watchErr:                      apierrors.NewBadRequest("invalid resourceVersion"),
			wantRVs:                       []string{"malformed", ""},
			wantDelete:                    true,
			wantRelist:                    true,
			wantRecoverySendInitialEvents: ptr.To(false),
		},
		{
			name:                          "Success: leaves another reader's replacement bookmark in place and relists instead of resuming from it",
			storedRV:                      "100",
			watchErr:                      apierrors.NewResourceExpired("the resourceVersion is too old"),
			replacementRV:                 "500",
			wantRVs:                       []string{"100", ""},
			wantRelist:                    true,
			wantRecoverySendInitialEvents: ptr.To(false),
		},
		{
			name:        "Success: without WatchList support an expired bookmark replays existing objects and relists",
			storedRV:    "500",
			watchErr:    apierrors.NewResourceExpired("the resourceVersion is too old"),
			noWatchList: true,
			// The first spawn is rejected for its WatchList options and retried as a standard watch.
			wantRVs:                       []string{"500", "500", ""},
			wantDelete:                    true,
			wantRelist:                    true,
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
			name:      "Error: returns bookmark cleanup failure",
			storedRV:  "500",
			watchErr:  apierrors.NewResourceExpired("the resourceVersion is too old"),
			deleteErr: errors.New("bookmark ConfigMap update failed"),
			wantRVs:   []string{"500"},
			wantErr:   true,
		},
		{
			name:    "Success: ignores bookmark load error",
			loadErr: errors.New("bookmark ConfigMap not found"),
			wantRVs: []string{""},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			key := namespaceGVR
			store := storefake.New(map[schema.GroupVersionResource]string{key: test.storedRV})
			store.SetLoadError(test.loadErr)
			store.SetDeleteError(test.deleteErr)
			capturedOptions := []metav1.ListOptions{}
			relisted := make(chan types.Retrieve, 1)

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
							t.Fatalf("Store() got err %v, want nil", err)
						}
					}
					return nil, test.watchErr
				}
				return watcherWithOnlyStop{}, nil
			}

			err := r.watch(ctx, types.RTNamespace, []watchSpec{{gvr: key, spawn: sp}})
			if test.wantErr && err == nil {
				t.Fatal("watch returned nil error, want non-nil")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("watch returned error: %v", err)
			}

			// The relist runs after watch() returns, so wait for it before cancelling.
			if test.wantRelist {
				waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
				rt, res := chans.Get(waitCtx, relisted)
				waitCancel()
				switch {
				case res != chans.ResultOK:
					t.Errorf("got no relist, want a relist of %v", types.RTNamespace)
				case rt != types.RTNamespace:
					t.Errorf("got relist of %v, want %v", rt, types.RTNamespace)
				}
			}
			cancel()
			_ = r.waitWatchers.Wait(t.Context())
			if _, ok, _ := chans.TryGet(relisted); ok && !test.wantRelist {
				t.Errorf("got a relist, want none")
			}

			if len(capturedOptions) != len(test.wantRVs) {
				t.Fatalf("got %d spawn calls, want %d", len(capturedOptions), len(test.wantRVs))
			}
			for i, wantRV := range test.wantRVs {
				if capturedOptions[i].ResourceVersion != wantRV {
					t.Errorf("spawn %d got ResourceVersion == %q, want %q", i, capturedOptions[i].ResourceVersion, wantRV)
				}
			}
			if test.wantRelist {
				last := capturedOptions[len(capturedOptions)-1]
				if diff := pretty.Compare(test.wantRecoverySendInitialEvents, last.SendInitialEvents); diff != "" {
					t.Errorf("recovery watch SendInitialEvents: -want/+got:\n%s", diff)
				}
			}
			if gotDelete := store.Deletes().Len() > 0; gotDelete != test.wantDelete {
				t.Errorf("got bookmark delete == %v, want %v", gotDelete, test.wantDelete)
			}
			if test.replacementRV != "" {
				if got := store.Stored(key); got != test.replacementRV {
					t.Errorf("got stored bookmark %q, want the replacement %q left in place", got, test.replacementRV)
				}
			}
		})
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

func TestWatchEventsStoresBookmarks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// bookmarks is how many bookmark events with the same resourceVersion the watcher sends.
		bookmarks  int
		storeErr   error
		wantStored string
		wantCalls  int
	}{
		{name: "Success: stores a bookmark", bookmarks: 1, wantStored: "700", wantCalls: 1},
		{name: "Success: skips storing a duplicate bookmark", bookmarks: 2, wantStored: "700", wantCalls: 1},
		{
			name:      "Success: retries storing a duplicate bookmark after a store error",
			bookmarks: 2,
			storeErr:  errors.New("bookmark ConfigMap update failed"),
			wantCalls: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store := storefake.New(map[schema.GroupVersionResource]string{})
			store.SetStoreError(test.storeErr)
			r := &Reader{bookmarkStore: store}
			watcher := watch.NewFakeWithChanSize(test.bookmarks, false)
			for i := 0; i < test.bookmarks; i++ {
				watcher.Action(watch.Bookmark, &fakeObject{rv: "700"})
			}
			watcher.Stop()

			resourceVersion, err := r.watchEvents(t.Context(), namespaceGVR, watcher)
			if err != nil {
				t.Fatalf("watchEvents() got err %v, want nil", err)
			}
			if resourceVersion != "700" {
				t.Fatalf("watchEvents() got resourceVersion %q, want %q", resourceVersion, "700")
			}
			if got := store.Stored(namespaceGVR); got != test.wantStored {
				t.Fatalf("stored bookmark got %q, want %q", got, test.wantStored)
			}
			if got := store.StoreCalls().Len(); got != test.wantCalls {
				t.Fatalf("Store() got %d calls, want %d", got, test.wantCalls)
			}
		})
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
		name        string
		ctx         context.Context
		ch          chan watch.Event
		wantRV      string
		event       watch.Event
		wantEntry   data.Entry
		wantErr     bool
		wantStopper bool
		// fullOut makes the output channel full before watchEvent is called, so sending an entry blocks.
		fullOut bool
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
	}

	type result struct {
		rv  string
		err error
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

		// watchEvent runs on the pool so that a watchEvent that never returns fails this test instead of hanging it.
		done := make(chan result, 1)
		context.Pool(t.Context()).Submit(
			t.Context(),
			func() {
				rv, err := r.watchEvent(test.ctx, test.ch, stopper)
				done <- result{rv: rv, err: err}
			},
		)
		guardCtx, guardCancel := context.WithTimeout(t.Context(), 5*time.Second)
		res, got := chans.Get(guardCtx, done)
		guardCancel()
		if got != chans.ResultOK {
			t.Errorf("TestWatchEvent(%s): watchEvent did not return within 5s", test.name)
			continue
		}
		gotRv, err := res.rv, res.err
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
		name             string
		watchEventErrors []error // Errors to return from watchEvents
		spawnErrors      []error // Errors to return when recreating watcher
		ctxTimeout       time.Duration
		expectExit       bool
	}{
		{
			name:             "Success: handles watcher until context cancelled",
			watchEventErrors: []error{nil, nil},
			ctxTimeout:       50 * time.Millisecond,
			expectExit:       true,
		},
		{
			name:             "Success: recovers from watchEvents error",
			watchEventErrors: []error{errors.New("watch error"), nil},
			spawnErrors:      []error{nil}, // Successfully recreates watcher
			ctxTimeout:       100 * time.Millisecond,
			expectExit:       true,
		},
		{
			name:             "Error: retries until context cancelled",
			watchEventErrors: []error{errors.New("watch error")},
			spawnErrors:      []error{errors.New("spawn error 1"), errors.New("spawn error 2"), errors.New("spawn error 3"), errors.New("spawn error 4"), errors.New("spawn error 5")},
			ctxTimeout:       100 * time.Millisecond,
			expectExit:       false, // Returns error when retry fails due to context
		},
	}

	for _, test := range tests {
		ctx, cancel := context.WithTimeout(t.Context(), test.ctxTimeout)
		defer cancel()

		watchEventIndex := 0
		spawnIndex := 0

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
				// Simulate watching events until context is done
				<-ctx.Done()
				return "", nil
			},
		}

		// Start connectWatcher to handle spawn requests
		context.Pool(t.Context()).Submit(t.Context(), func() { r.connectWatcher(ctx, r.spawnCh) })

		// Create spawn function that returns errors as specified
		sp := func(options metav1.ListOptions) (watch.Interface, error) {
			if spawnIndex < len(test.spawnErrors) {
				err := test.spawnErrors[spawnIndex]
				spawnIndex++
				if err != nil {
					return nil, err
				}
			}
			return &fakeWatcher{}, nil
		}

		// Create initial watcher
		w := &fakeWatcher{}

		err := r.handleWatcher(ctx, types.RTNamespace, namespaceGVR, "", w, sp)

		if test.expectExit && err != nil {
			t.Errorf("TestHandleWatcher(%s): unexpected error: %v", test.name, err)
		}
		if !test.expectExit && err == nil {
			t.Errorf("TestHandleWatcher(%s): expected error but got nil", test.name)
		}
	}
}

func TestGetWatcher(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		cancelBefore bool
		spawn        spawnWatcher
		setupFn      func(r *Reader, ctx context.Context)
		wantErr      bool
		errMsg       string
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
				return nil, errors.New("connection failed")
			},
			setupFn: func(r *Reader, ctx context.Context) {
				context.Pool(t.Context()).Submit(t.Context(), func() { r.connectWatcher(ctx, r.spawnCh) })
			},
			wantErr: true,
			errMsg:  "connection failed",
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
			errMsg:  "context canceled",
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
		case test.wantErr && err == nil:
			t.Errorf("TestGetWatcher(%s): got err == nil, want err != nil", test.name)
		case !test.wantErr && err != nil:
			t.Errorf("TestGetWatcher(%s): got err == %v, want err == nil", test.name, err)
		case test.wantErr && err != nil:
			if test.errMsg != "" && !strings.Contains(err.Error(), test.errMsg) {
				t.Errorf("TestGetWatcher(%s): error message doesn't contain %q, got %v", test.name, test.errMsg, err)
			}
		}

		if !test.wantErr && w == nil {
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
		name                    string
		bookmarking             bool
		bookmarkRV              string
		spawnErr                error
		replacementRV           string
		wantRVOnFirstReconnect  string
		wantRVOnSecondReconnect string
		wantDelete              bool
		wantPermanent           bool
		// wantRelist is set when a rejected bookmark must be recovered with a snapshot relist, with the reconnect
		// made at the most recent resourceVersion without initial events.
		wantRelist bool
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
			wantRelist:              true,
		},
		{
			name:                    "Success: gone resource version resets bookmark",
			bookmarking:             true,
			bookmarkRV:              "100",
			spawnErr:                apierrors.NewGone("the resourceVersion is gone"),
			wantRVOnFirstReconnect:  "100",
			wantRVOnSecondReconnect: "",
			wantDelete:              true,
			wantRelist:              true,
		},
		{
			name:                    "Success: malformed resource version resets bookmark",
			bookmarking:             true,
			bookmarkRV:              "malformed",
			spawnErr:                apierrors.NewBadRequest("invalid resourceVersion"),
			wantRVOnFirstReconnect:  "malformed",
			wantRVOnSecondReconnect: "",
			wantDelete:              true,
			wantRelist:              true,
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
			name:                    "Success: stale watcher leaves another reader's replacement bookmark in place and relists instead of resuming from it",
			bookmarking:             true,
			bookmarkRV:              "100",
			spawnErr:                apierrors.NewResourceExpired("the resourceVersion is too old"),
			replacementRV:           "500",
			wantRVOnFirstReconnect:  "100",
			wantRVOnSecondReconnect: "",
			wantRelist:              true,
		},
		{
			name:                    "Success: no bookmarking does not set resource version",
			bookmarking:             false,
			bookmarkRV:              "100",
			wantRVOnFirstReconnect:  "",
			wantRVOnSecondReconnect: "",
		},
		{
			name:                   "Error: forbidden reconnect is permanent",
			bookmarking:            true,
			bookmarkRV:             "100",
			spawnErr:               apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", errors.New("forbidden")),
			wantRVOnFirstReconnect: "100",
			wantPermanent:          true,
		},
	}

	for _, test := range tests {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		watchCallCount := 0
		var capturedOptions []metav1.ListOptions
		spawnCallCount := 0
		store := storefake.New(map[schema.GroupVersionResource]string{namespaceGVR: test.bookmarkRV})
		relisted := make(chan types.Retrieve, 1)

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
					return test.bookmarkRV, nil
				case 2:
					return "", nil
				default:
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
			func() { done <- r.handleWatcher(ctx, types.RTNamespace, namespaceGVR, "", &fakeWatcher{}, sp) },
		)

		time.Sleep(200 * time.Millisecond)
		cancel()

		err := <-done
		_ = r.waitWatchers.Wait(t.Context())
		// A relist after cancel returns before calling List, so this only sees relists made while running.
		if _, gotRelist, _ := chans.TryGet(relisted); gotRelist != test.wantRelist {
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): got relist == %v, want %v", test.name, gotRelist, test.wantRelist)
		}
		if got := errors.Is(err, exponential.ErrPermanent); got != test.wantPermanent {
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): permanent error got %v, want %v: %v", test.name, got, test.wantPermanent, err)
		}

		if len(capturedOptions) < 1 {
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): expected at least 1 spawn call, got %d", test.name, len(capturedOptions))
			continue
		}

		if capturedOptions[0].ResourceVersion != test.wantRVOnFirstReconnect {
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): first reconnect got ResourceVersion == %q, want %q", test.name, capturedOptions[0].ResourceVersion, test.wantRVOnFirstReconnect)
		}

		if test.spawnErr != nil && len(capturedOptions) >= 2 {
			if capturedOptions[1].ResourceVersion != test.wantRVOnSecondReconnect {
				t.Errorf("TestHandleWatcherBookmarkReconnection(%s): second reconnect got ResourceVersion == %q, want %q", test.name, capturedOptions[1].ResourceVersion, test.wantRVOnSecondReconnect)
			}
			if test.wantRelist {
				if diff := pretty.Compare(ptr.To(false), capturedOptions[1].SendInitialEvents); diff != "" {
					t.Errorf("TestHandleWatcherBookmarkReconnection(%s): recovery reconnect SendInitialEvents: -want/+got:\n%s", test.name, diff)
				}
			}
		}
		if gotDelete := store.Deletes().Len() > 0; gotDelete != test.wantDelete {
			t.Errorf("TestHandleWatcherBookmarkReconnection(%s): got bookmark delete == %v, want %v", test.name, gotDelete, test.wantDelete)
		}
	}
}
