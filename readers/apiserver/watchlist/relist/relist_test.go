package relist

import (
	"errors"
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/readers/apiserver/watchlist/types"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/retry/exponential"
	"github.com/gostdlib/base/values/generics/promises"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	tests := []struct {
		name      string
		clientset ClientsetInterface
		wantErr   bool
	}{
		{
			name:      "valid clientset",
			clientset: fake.NewSimpleClientset(),
			wantErr:   false,
		},
		{
			name:      "nil clientset",
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
		name              string
		rt                types.Retrieve
		setupPagers       func() map[types.Retrieve][]listPage
		setupClientset    func() *fake.Clientset
		contextSetup      func() (context.Context, context.CancelFunc)
		wantEntries       int
		wantCallErr       bool
		wantStreamErr     bool
		wantCancellation  bool
		validateEntries   func([]data.Entry, *testing.T)
		validateBehavior  func(context.Context, context.CancelFunc, chan promises.Response[data.Entry], *testing.T)
		expectChannelClosed bool
	}{
		{
			name: "successful list",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve][]listPage {
				return map[types.Retrieve][]listPage{
					types.RTPod: {fakeListPage(entries, "", nil)},
				}
			},
			wantEntries: 3,
		},
		{
			name: "empty list",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve][]listPage {
				return map[types.Retrieve][]listPage{
					types.RTPod: {fakeListPage([]data.Entry{}, "", nil)},
				}
			},
		},
		{
			name: "unknown resource type",
			rt:   types.Retrieve(999), // Invalid resource type
			setupPagers: func() map[types.Retrieve][]listPage {
				return map[types.Retrieve][]listPage{
					types.RTPod: {fakeListPage(entries, "", nil)},
				}
			},
			wantCallErr: true,
		},
		{
			name: "pager returns error",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve][]listPage {
				return map[types.Retrieve][]listPage{
					types.RTPod: {fakeListPage(nil, "", errors.New("api error"))},
				}
			},
			wantStreamErr: true, // Error comes through channel
		},
		{
			name: "pagination",
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
			name: "context cancellation",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve][]listPage {
				return map[types.Retrieve][]listPage{
					types.RTPod: {func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
						select {
						case <-ctx.Done():
							return nil, "", context.Cause(ctx)
						case <-time.After(100 * time.Millisecond):
							return createTestEntries(createTestPods(1)), "", nil
						}
					}},
				}
			},
			contextSetup: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				return ctx, cancel
			},
			validateBehavior: func(ctx context.Context, cancel context.CancelFunc, ch chan promises.Response[data.Entry], t *testing.T) {
				// Cancel context immediately
				cancel()

				// Read from channel - should get an error
				var gotError bool
				for response := range ch {
					if response.Err != nil {
						gotError = true
						// Accept either context.Canceled or ErrRetryCanceled (when using WithTesting())
						if !errors.Is(response.Err, context.Canceled) && !errors.Is(response.Err, exponential.ErrRetryCanceled) {
							t.Errorf("expected context.Canceled or ErrRetryCanceled error, got %v", response.Err)
						}
						break
					}
				}

				if !gotError {
					t.Error("expected error due to context cancellation")
				}
			},
		},
		{
			name: "context cancellation during iteration",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve][]listPage {
				pods := createTestPods(10)
				entries := createTestEntries(pods)
				return map[types.Retrieve][]listPage{
					types.RTPod: {func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
						return entries, "", nil
					}},
				}
			},
			contextSetup: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			validateBehavior: func(ctx context.Context, cancel context.CancelFunc, ch chan promises.Response[data.Entry], t *testing.T) {
				// Read one entry, then cancel
				entryCount := 0
				var gotError bool
				for response := range ch {
					if response.Err != nil {
						gotError = true
						break
					}
					entryCount++
					if entryCount == 1 {
						cancel() // Cancel after reading first entry
					}
				}

				if !gotError {
					t.Error("expected error due to context cancellation during iteration")
				}

				if entryCount == 0 {
					t.Error("should have read at least one entry before cancellation")
				}
			},
		},
		{
			name: "integration with fake clientset",
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
			name: "RBAC integration with all four types",
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
			name: "multiple listers for single resource type",
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
			name: "multiple listers with error in second lister",
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
			name: "channel closure",
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
			var r *Relister
			var err error
			
			// Setup relister
			if test.setupClientset != nil {
				clientset := test.setupClientset()
				r, err = New(clientset)
				if err != nil {
					t.Fatalf("New() error = %v", err)
				}
			} else if test.setupPagers != nil {
				r = &Relister{pagers: test.setupPagers()}
			}

			// Setup context
			ctx := context.Background()
			var cancel context.CancelFunc
			if test.contextSetup != nil {
				ctx, cancel = test.contextSetup()
			}

			ch, err := r.List(ctx, test.rt)
			if ec.continueTest(test.name, test.wantCallErr, err, t) {
				if !errors.Is(err, exponential.ErrPermanent) {
					t.Errorf("wanted error was not permanent")
				}
				return
			}

			// Use custom validation if provided
			if test.validateBehavior != nil {
				test.validateBehavior(ctx, cancel, ch, t)
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
				select {
				case response, ok := <-ch:
					if ok {
						t.Errorf("channel should be closed, but received: %v", response)
					}
				default:
					// This is expected - channel is closed
				}
			}
		})
	}
}


func TestRetryableList(t *testing.T) {
	t.Parallel()

	pods := createTestPods(3)
	entries := createTestEntries(pods)

	tests := []struct {
		name         string
		lister       listPage
		wantErr      bool
		wantEntries  []data.Entry
		wantContinue string
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
			name: "Error: context cancelled",
			lister: func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
				select {
				case <-ctx.Done():
					return nil, "", ctx.Err()
				default:
					return nil, "", errors.New("should not reach here")
				}
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
	}

	for _, test := range tests {
		ctx := t.Context()
		if test.name == "Error: context cancelled" {
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

	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
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

	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
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
