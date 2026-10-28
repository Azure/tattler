package relist

import (
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/readers/apiserver/watchlist/types"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/errors"
	"github.com/gostdlib/base/retry/exponential"
	"github.com/gostdlib/base/values/chans"
	"github.com/gostdlib/base/values/generics/promises"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func init() {
	back = exponential.Must(exponential.New(exponential.WithTesting()))
}

// fakeListPage creates a test implementation of listPage
func fakeListPage(entries []data.Entry, continueToken string, err error) listPage {
	return func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
		return entries, continueToken, err
	}
}

// createTestPods creates test pods for testing
func createTestPods(count int) []*corev1.Pod {
	pods := make([]*corev1.Pod, count)
	for i := 0; i < count; i++ {
		pods[i] = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pod-" + string(rune('a'+i)),
				Namespace: "default",
				UID:       k8stypes.UID("uid-" + string(rune('a'+i))),
			},
		}
	}
	return pods
}

// createTestEntries creates test data.Entry objects from pods
func createTestEntries(pods []*corev1.Pod) []data.Entry {
	entries := make([]data.Entry, len(pods))
	for i, pod := range pods {
		entries[i] = data.MustNewEntry(pod, data.STWatchList, data.CTSnapshot)
	}
	return entries
}

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		clientset ClientsetInterface
		wantErr   bool
	}{
		{
			name:      "Success: a clientset gives a relister with pagers",
			clientset: fake.NewSimpleClientset(),
			wantErr:   false,
		},
		{
			name:      "Error: the clientset is nil",
			clientset: nil,
			wantErr:   true,
		},
	}

	ec := errCheck{testName: "TestNew"}

	for _, test := range tests {
		relister, err := New(test.clientset)

		if ec.continueTest(test.name, test.wantErr, err, t) {
			continue
		}

		if relister == nil {
			t.Errorf("TestNew(%s): returned nil relister without error", test.name)
			continue
		}
		if relister.pagers == nil {
			t.Errorf("TestNew(%s): relister.pagers is nil", test.name)
			continue
		}
		if len(relister.pagers) == 0 {
			t.Errorf("TestNew(%s): relister.pagers is empty", test.name)
			continue
		}
	}
}

func TestMakePagers(t *testing.T) {
	t.Parallel()

	clientset := fake.NewSimpleClientset()
	pagers := makePagers(clientset)

	// Test that all expected resource types have pagers
	expectedTypes := []types.Retrieve{
		types.RTNode,
		types.RTPod,
		types.RTNamespace,
		types.RTPersistentVolume,
		types.RTRBAC,
		types.RTService,
		types.RTDeployment,
		types.RTIngressController,
		types.RTEndpoint,
	}

	for _, rt := range expectedTypes {
		if _, exists := pagers[rt]; !exists {
			t.Errorf("TestMakePagers: missing pager for resource type %v", rt)
		}
	}

	if len(pagers) != len(expectedTypes) {
		t.Errorf("TestMakePagers: got %d pagers, want %d", len(pagers), len(expectedTypes))
	}

	// Test that pagers are functional (not nil or empty)
	for rt, pagerSlice := range pagers {
		if len(pagerSlice) == 0 {
			t.Errorf("TestMakePagers: pager slice for %v is empty", rt)
		}
		for i, pager := range pagerSlice {
			if pager == nil {
				t.Errorf("TestMakePagers: pager[%d] for %v is nil", i, rt)
			}
		}
	}
}

func TestRelister(t *testing.T) {
	t.Parallel()

	pods := createTestPods(3)
	entries := createTestEntries(pods)

	tests := []struct {
		name                string
		rt                  types.Retrieve
		setupPagers         func() map[types.Retrieve][]listPage
		setupClientset      func() *fake.Clientset
		wantEntries         int
		wantCallErr         bool
		wantStreamErr       bool
		validateEntries     func([]data.Entry, *testing.T)
		expectChannelClosed bool
	}{
		{
			name: "Success: lists every entry",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve][]listPage {
				return map[types.Retrieve][]listPage{
					types.RTPod: {fakeListPage(entries, "", nil)},
				}
			},
			wantEntries: 3,
		},
		{
			name: "Success: an empty list closes with no entries",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve][]listPage {
				return map[types.Retrieve][]listPage{
					types.RTPod: {fakeListPage([]data.Entry{}, "", nil)},
				}
			},
		},
		{
			name: "Error: the resource type has no pager",
			rt:   types.Retrieve(999), // Invalid resource type
			setupPagers: func() map[types.Retrieve][]listPage {
				return map[types.Retrieve][]listPage{
					types.RTPod: {fakeListPage(entries, "", nil)},
				}
			},
			wantCallErr: true,
		},
		{
			name: "Error: the pager fails and the error is sent on the channel",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve][]listPage {
				return map[types.Retrieve][]listPage{
					types.RTPod: {fakeListPage(nil, "", errors.New("api error"))},
				}
			},
			wantStreamErr: true, // Error comes through channel
		},
		{
			name: "Success: follows continue tokens across pages",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve][]listPage {
				pods1 := createTestPods(2)
				entries1 := createTestEntries(pods1)
				pods2 := createTestPods(1)
				entries2 := createTestEntries(pods2)

				callCount := 0
				return map[types.Retrieve][]listPage{
					types.RTPod: {func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
						callCount++
						switch callCount {
						case 1:
							return entries1, "page2", nil
						case 2:
							return entries2, "", nil // Empty continue means no more pages
						default:
							return nil, "", errors.New("too many calls")
						}
					}},
				}
			},
			wantEntries: 3, // 2 + 1 from pagination
		},
		{
			name: "Success: lists pods from a fake clientset as watchlist snapshots",
			rt:   types.RTPod,
			setupClientset: func() *fake.Clientset {
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

				return fake.NewSimpleClientset(pod1, pod2)
			},
			wantEntries: 2,
			validateEntries: func(entries []data.Entry, t *testing.T) {
				// Verify entries have correct properties
				for i, entry := range entries {
					if entry.SourceType() != data.STWatchList {
						t.Errorf("entry[%d].SourceType() = %v, want %v", i, entry.SourceType(), data.STWatchList)
					}
					if entry.ChangeType() != data.CTSnapshot {
						t.Errorf("entry[%d].ChangeType() = %v, want %v", i, entry.ChangeType(), data.CTSnapshot)
					}
					if entry.ObjectType() != data.OTPod {
						t.Errorf("entry[%d].ObjectType() = %v, want %v", i, entry.ObjectType(), data.OTPod)
					}
				}
			},
		},
		{
			name: "Success: RBAC lists all four RBAC types",
			rt:   types.RTRBAC,
			setupClientset: func() *fake.Clientset {
				role := &rbacv1.Role{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-role",
						Namespace: "default",
						UID:       k8stypes.UID("role-uid"),
					},
				}
				roleBinding := &rbacv1.RoleBinding{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-rolebinding",
						Namespace: "default",
						UID:       k8stypes.UID("rolebinding-uid"),
					},
				}
				clusterRole := &rbacv1.ClusterRole{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-clusterrole",
						UID:  k8stypes.UID("clusterrole-uid"),
					},
				}
				clusterRoleBinding := &rbacv1.ClusterRoleBinding{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-clusterrolebinding",
						UID:  k8stypes.UID("clusterrolebinding-uid"),
					},
				}
				return fake.NewSimpleClientset(role, roleBinding, clusterRole, clusterRoleBinding)
			},
			wantEntries: 4,
			validateEntries: func(entries []data.Entry, t *testing.T) {
				// Track which object types we've seen
				seenTypes := make(map[data.ObjectType]bool)
				for _, entry := range entries {
					if entry.SourceType() != data.STWatchList {
						t.Errorf("entry.SourceType() = %v, want %v", entry.SourceType(), data.STWatchList)
					}
					if entry.ChangeType() != data.CTSnapshot {
						t.Errorf("entry.ChangeType() = %v, want %v", entry.ChangeType(), data.CTSnapshot)
					}
					seenTypes[entry.ObjectType()] = true
				}
				// Verify we got all four RBAC types
				expectedTypes := []data.ObjectType{data.OTRole, data.OTRoleBinding, data.OTClusterRole, data.OTClusterRoleBinding}
				for _, ot := range expectedTypes {
					if !seenTypes[ot] {
						t.Errorf("missing expected object type %v in RBAC entries", ot)
					}
				}
			},
		},
		{
			name: "Success: a type with several listers sends every lister's entries",
			rt:   types.RTRBAC,
			setupPagers: func() map[types.Retrieve][]listPage {
				// Simulate RBAC with multiple listers (Roles, RoleBindings, ClusterRoles, ClusterRoleBindings)
				pods1 := createTestPods(2)
				entries1 := createTestEntries(pods1)
				pods2 := createTestPods(3)
				entries2 := createTestEntries(pods2)
				pods3 := createTestPods(1)
				entries3 := createTestEntries(pods3)
				return map[types.Retrieve][]listPage{
					types.RTRBAC: {
						fakeListPage(entries1, "", nil),
						fakeListPage(entries2, "", nil),
						fakeListPage(entries3, "", nil),
					},
				}
			},
			wantEntries: 6, // 2 + 3 + 1 from three listers
		},
		{
			name: "Error: a failing lister stops the list after the earlier listers' entries",
			rt:   types.RTRBAC,
			setupPagers: func() map[types.Retrieve][]listPage {
				pods1 := createTestPods(2)
				entries1 := createTestEntries(pods1)
				return map[types.Retrieve][]listPage{
					types.RTRBAC: {
						fakeListPage(entries1, "", nil),
						fakeListPage(nil, "", errors.New("second lister error")),
						fakeListPage(createTestEntries(createTestPods(1)), "", nil), // Should not be reached
					},
				}
			},
			wantEntries:   2, // Only first lister's entries before error
			wantStreamErr: true,
		},
		{
			name: "Success: the channel is closed after the last entry",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve][]listPage {
				return map[types.Retrieve][]listPage{
					types.RTPod: {fakeListPage(createTestEntries(createTestPods(1)), "", nil)},
				}
			},
			wantEntries:         1,
			expectChannelClosed: true,
		},
	}

	ec := errCheck{testName: "TestRelister"}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var r *Relister
			var err error

			// Setup relister
			switch {
			case test.setupClientset != nil:
				r, err = New(test.setupClientset())
				if err != nil {
					t.Fatalf("TestRelister(%s): New() error = %v", test.name, err)
				}
			case test.setupPagers != nil:
				r = &Relister{pagers: test.setupPagers()}
			}

			ch, err := r.List(t.Context(), test.rt)
			// relistType's retry stops on exponential.ErrPermanent, and an unknown type never succeeds.
			if err != nil && !errors.Is(err, exponential.ErrPermanent) {
				t.Errorf("TestRelister(%s): List() error was not permanent", test.name)
			}
			if ec.continueTest(test.name, test.wantCallErr, err, t) {
				return
			}

			// Standard validation
			var receivedEntries []data.Entry
			var finalErr error

			for response := range ch {
				if response.Err != nil {
					finalErr = response.Err
					break
				}
				receivedEntries = append(receivedEntries, response.V)
			}

			if ec.continueTest(test.name, test.wantStreamErr, finalErr, t) {
				return
			}

			if len(receivedEntries) != test.wantEntries {
				t.Errorf("got %d entries, want %d", len(receivedEntries), test.wantEntries)
			}

			// Custom entry validation
			if test.validateEntries != nil {
				test.validateEntries(receivedEntries, t)
			}

			// Check channel closure if needed
			if test.expectChannelClosed {
				guard, guardCancel := context.WithTimeout(t.Context(), 5*time.Second)
				response, res := chans.Get(guard, ch)
				guardCancel()
				if res != chans.ResultClosed {
					t.Errorf("TestRelister(%s): got chans.Get() result %v (%v), want %v", test.name, res, response, chans.ResultClosed)
				}
			}
		})
	}
}

// TestListCancel covers List when its context is cancelled. Unlike TestRelister, the reader here cancels at a chosen
// point and the channel must still close.
func TestListCancel(t *testing.T) {
	t.Parallel()

	// waitingPager returns one entry after 100ms, or the context's error if it is cancelled first.
	waitingPager := func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
		if _, res := chans.Get(ctx, time.After(100*time.Millisecond)); res != chans.ResultOK {
			return nil, "", context.Cause(ctx)
		}
		return createTestEntries(createTestPods(1)), "", nil
	}

	tests := []struct {
		name  string
		pager listPage
		// cancelBeforeList cancels the context before List is called.
		cancelBeforeList bool
		// cancelAfter cancels the context once this many entries have been read. 0 cancels right after List returns.
		cancelAfter int
		// wantErr is whether an error must arrive before the channel closes. A lister that finds the channel full after
		// cancel may drop its error, so rows where that can happen leave this false and only require the channel to close.
		wantErr bool
		// minEntries is the fewest entries that must arrive before the channel closes.
		minEntries int
	}{
		{
			name:             "Success: cancelling before List sends an error and closes the channel",
			pager:            fakeListPage(createTestEntries(createTestPods(1)), "", nil),
			cancelBeforeList: true,
			wantErr:          true,
		},
		{
			name:    "Success: cancelling while the pager is waiting sends an error and closes the channel",
			pager:   waitingPager,
			wantErr: true,
		},
		{
			name:        "Success: cancelling partway through the entries closes the channel",
			pager:       fakeListPage(createTestEntries(createTestPods(10)), "", nil),
			cancelAfter: 1,
			minEntries:  1,
		},
	}

	for _, test := range tests {
		r := &Relister{pagers: map[types.Retrieve][]listPage{types.RTPod: {test.pager}}}

		ctx, cancel := context.WithCancel(t.Context())
		if test.cancelBeforeList {
			cancel()
		}
		ch, err := r.List(ctx, types.RTPod)
		if err != nil {
			cancel()
			t.Errorf("TestListCancel(%s): got err == %s, want err == nil", test.name, err)
			continue
		}
		if test.cancelAfter == 0 {
			cancel()
		}

		// guard fails the row instead of hanging the test if the channel never closes.
		guard, guardCancel := context.WithTimeout(t.Context(), 5*time.Second)
		entries := 0
		gotErr := false
		closed := false
		for !closed {
			resp, res := chans.Get(guard, ch)
			switch {
			case res == chans.ResultClosed:
				closed = true
			case !res.OK():
				t.Errorf("TestListCancel(%s): channel did not close within 5s", test.name)
				closed = true
			case resp.Err != nil:
				gotErr = true
			default:
				entries++
				if entries == test.cancelAfter {
					cancel()
				}
			}
		}
		guardCancel()
		cancel()

		if test.wantErr && !gotErr {
			t.Errorf("TestListCancel(%s): got no error before the channel closed, want one", test.name)
		}
		if entries < test.minEntries {
			t.Errorf("TestListCancel(%s): got %d entries, want at least %d", test.name, entries, test.minEntries)
		}
	}
}

func TestRetryableList(t *testing.T) {
	t.Parallel()

	pods := createTestPods(3)
	entries := createTestEntries(pods)

	// countingLister returns a listPage that always fails with err and a func reporting how often it was called.
	countingLister := func(err error) (listPage, func() int) {
		calls := 0
		lister := func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
			calls++
			return nil, "", err
		}
		return lister, func() int { return calls }
	}
	gr := schema.GroupResource{Resource: "pods"}
	forbiddenLister, forbiddenCalls := countingLister(apierrors.NewForbidden(gr, "", errors.New("rbac")))
	timeoutLister, timeoutCalls := countingLister(apierrors.NewServerTimeout(gr, "list", 1))

	tests := []struct {
		name         string
		lister       listPage
		wantErr      bool
		wantEntries  []data.Entry
		wantContinue string
		// cancelled cancels the context before retryableList is called.
		cancelled bool
		// calls, when set, reports how many times lister was called and must equal wantCalls.
		calls     func() int
		wantCalls int
	}{
		{
			name: "Success: returns result on first try",
			lister: func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
				return entries, "continue-token", nil
			},
			wantErr:      false,
			wantEntries:  entries,
			wantContinue: "continue-token",
		},
		{
			name: "Success: retries and succeeds",
			lister: func() listPage {
				attempts := 0
				return func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
					attempts++
					if attempts < 3 {
						return nil, "", errors.New("temporary error")
					}
					return entries, "token-after-retry", nil
				}
			}(),
			wantErr:      false,
			wantEntries:  entries,
			wantContinue: "token-after-retry",
		},
		{
			name: "Error: fails after max retries",
			lister: func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
				return nil, "", errors.New("persistent error")
			},
			wantErr: true,
		},
		{
			name:      "Error: context cancelled",
			cancelled: true,
			lister: func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
				if err := ctx.Err(); err != nil {
					return nil, "", err
				}
				return nil, "", errors.New("should not reach here")
			},
			wantErr: true,
		},
		{
			name: "Success: partial data with continue token",
			lister: func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
				// Return first two entries with continue token
				return entries[:2], "more-data", nil
			},
			wantErr:      false,
			wantEntries:  entries[:2],
			wantContinue: "more-data",
		},
		{
			name:      "Error: forbidden is permanent and not retried",
			lister:    forbiddenLister,
			wantErr:   true,
			calls:     forbiddenCalls,
			wantCalls: 1,
		},
		{
			name:      "Error: server timeout is retried until max attempts",
			lister:    timeoutLister,
			wantErr:   true,
			calls:     timeoutCalls,
			wantCalls: 10,
		},
	}

	for _, test := range tests {
		ctx := t.Context()
		if test.cancelled {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		}

		options := metav1.ListOptions{}
		result, continueToken, err := retryableList(ctx, test.lister, options)

		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestRetryableList(%s): got err == nil, want err != nil", test.name)
		case !test.wantErr && err != nil:
			t.Errorf("TestRetryableList(%s): got err == %v, want err == nil", test.name, err)
		}

		if test.calls != nil && test.calls() != test.wantCalls {
			t.Errorf("TestRetryableList(%s): got %d calls to the lister, want %d", test.name, test.calls(), test.wantCalls)
		}

		if !test.wantErr {
			if len(result) != len(test.wantEntries) {
				t.Errorf("TestRetryableList(%s): got %d entries, want %d", test.name, len(result), len(test.wantEntries))
			}

			for i, entry := range result {
				if i < len(test.wantEntries) {
					if entry.UID() != test.wantEntries[i].UID() {
						t.Errorf("TestRetryableList(%s): entry[%d] UID mismatch: got %s, want %s",
							test.name, i, entry.UID(), test.wantEntries[i].UID())
					}
				}
			}

			if continueToken != test.wantContinue {
				t.Errorf("TestRetryableList(%s): continue token mismatch: got %q, want %q",
					test.name, continueToken, test.wantContinue)
			}
		}
	}
}

func BenchmarkRelisterList(b *testing.B) {
	pods := createTestPods(100)
	entries := createTestEntries(pods)

	r := &Relister{
		pagers: map[types.Retrieve][]listPage{
			types.RTPod: {fakeListPage(entries, "", nil)},
		},
	}

	ctx := b.Context()

	b.ResetTimer()
	for b.Loop() {
		ch, err := r.List(ctx, types.RTPod)
		if err != nil {
			b.Fatalf("BenchmarkRelisterList: List() error = %v", err)
		}

		// Consume all entries
		for response := range ch {
			if response.Err != nil {
				b.Fatalf("BenchmarkRelisterList: unexpected error: %v", response.Err)
			}
		}
	}
}

func BenchmarkRelisterListWithPagination(b *testing.B) {
	pods1 := createTestPods(50)
	entries1 := createTestEntries(pods1)
	pods2 := createTestPods(50)
	entries2 := createTestEntries(pods2)

	callCount := 0
	paginatedLister := func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
		callCount++
		if callCount%2 == 1 {
			return entries1, "page2", nil
		}
		return entries2, "", nil
	}

	r := &Relister{
		pagers: map[types.Retrieve][]listPage{
			types.RTPod: {paginatedLister},
		},
	}

	ctx := b.Context()

	b.ResetTimer()
	for b.Loop() {
		callCount = 0 // Reset for each benchmark iteration
		ch, err := r.List(ctx, types.RTPod)
		if err != nil {
			b.Fatalf("BenchmarkRelisterListWithPagination: List() error = %v", err)
		}

		// Consume all entries
		for response := range ch {
			if response.Err != nil {
				b.Fatalf("BenchmarkRelisterListWithPagination: unexpected error: %v", response.Err)
			}
		}
	}
}

func TestSendErr(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	entry := promises.Response[data.Entry]{V: createTestEntries(createTestPods(1))[0]}

	tests := []struct {
		name string
		// full fills the channel before sendErr is called, as when the reader stopped receiving.
		full bool
		// wantErr is true when the value left at the front of the channel should be the error.
		wantErr bool
	}{
		{
			name:    "Success: a reader that is still receiving gets the error after ctx is done",
			wantErr: true,
		},
		{
			name: "Success: a full channel after ctx is done returns without blocking and drops the error",
			full: true,
		},
	}

	for _, test := range tests {
		ch := make(chan promises.Response[data.Entry], 1)
		if test.full {
			ch <- entry
		}

		// Before sendErr, the lister sent the error unconditionally and blocked forever on a full channel.
		done := make(chan struct{})
		context.Pool(t.Context()).Submit(
			t.Context(),
			func() {
				defer close(done)
				sendErr(cancelled, ch, errors.New("list failed"))
			},
		)
		guard, guardCancel := context.WithTimeout(t.Context(), 5*time.Second)
		_, res := chans.Get(guard, done)
		guardCancel()
		if res != chans.ResultClosed {
			t.Errorf("TestSendErr(%s): sendErr() did not return", test.name)
			continue
		}

		got := <-ch
		if (got.Err != nil) != test.wantErr {
			t.Errorf("TestSendErr(%s): got err == %v, want err != nil == %v", test.name, got.Err, test.wantErr)
		}
	}
}
