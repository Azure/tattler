package relist

import (
	"errors"
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/readers/apiserver/watchlist/internal/types"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/retry/exponential"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

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

	// Test that pagers are functional (not nil)
	for rt, pager := range pagers {
		if pager == nil {
			t.Errorf("TestMakePagers: pager for %v is nil", rt)
		}
	}
}

func TestRelisterList(t *testing.T) {
	pods := createTestPods(3)
	entries := createTestEntries(pods)

	tests := []struct {
		name          string
		rt            types.Retrieve
		setupPagers   func() map[types.Retrieve]listPage
		wantEntries   int
		wantCallErr   bool
		wantStreamErr bool
	}{
		{
			name: "successful list",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve]listPage {
				return map[types.Retrieve]listPage{
					types.RTPod: fakeListPage(entries, "", nil),
				}
			},
			wantEntries: 3,
		},
		{
			name: "empty list",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve]listPage {
				return map[types.Retrieve]listPage{
					types.RTPod: fakeListPage([]data.Entry{}, "", nil),
				}
			},
		},
		{
			name: "unknown resource type",
			rt:   types.Retrieve(999), // Invalid resource type
			setupPagers: func() map[types.Retrieve]listPage {
				return map[types.Retrieve]listPage{
					types.RTPod: fakeListPage(entries, "", nil),
				}
			},
			wantCallErr: true,
		},
		{
			name: "pager returns error",
			rt:   types.RTPod,
			setupPagers: func() map[types.Retrieve]listPage {
				return map[types.Retrieve]listPage{
					types.RTPod: fakeListPage(nil, "", errors.New("api error")),
				}
			},
			wantStreamErr: true, // Error comes through channel
		},
	}

	ec := errCheck{testName: "TestRelisterList"}

	for _, test := range tests {
		r := &Relister{pagers: test.setupPagers()}

		ctx := context.Background()
		ch, err := r.List(ctx, test.rt)
		if ec.continueTest(test.name, test.wantCallErr, err, t) {
			if !errors.Is(err, exponential.ErrPermanent) {
				t.Errorf("TestRelisterList(%s): wanted error was not permanent", test.name)
			}
			continue
		}

		// Read all entries from channel
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
			continue
		}

		if len(receivedEntries) != test.wantEntries {
			t.Errorf("TestRelisterList(%s): got %d entries, want %d", test.name, len(receivedEntries), test.wantEntries)
		}
	}
}

func TestRelisterListPagination(t *testing.T) {
	pods1 := createTestPods(2)
	entries1 := createTestEntries(pods1)

	pods2 := createTestPods(1)
	entries2 := createTestEntries(pods2)

	callCount := 0
	paginatedLister := func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
		callCount++
		switch callCount {
		case 1:
			if opts.Continue != "" {
				t.Errorf("TestRelisterListPagination: first call should have empty continue token, got %s", opts.Continue)
			}
			return entries1, "page2", nil
		case 2:
			if opts.Continue != "page2" {
				t.Errorf("TestRelisterListPagination: second call should have continue token 'page2', got %s", opts.Continue)
			}
			return entries2, "", nil // Empty continue means no more pages
		default:
			t.Errorf("TestRelisterListPagination: unexpected call %d to lister", callCount)
			return nil, "", errors.New("too many calls")
		}
	}

	r := &Relister{
		pagers: map[types.Retrieve]listPage{
			types.RTPod: paginatedLister,
		},
	}

	ctx := context.Background()
	ch, err := r.List(ctx, types.RTPod)
	if err != nil {
		t.Fatalf("TestRelisterListPagination: List() error = %v", err)
	}

	var receivedEntries []data.Entry
	for response := range ch {
		if response.Err != nil {
			t.Fatalf("TestRelisterListPagination: unexpected error from channel: %v", response.Err)
		}
		receivedEntries = append(receivedEntries, response.V)
	}

	expectedTotal := len(entries1) + len(entries2)
	if len(receivedEntries) != expectedTotal {
		t.Errorf("TestRelisterListPagination: got %d entries, want %d", len(receivedEntries), expectedTotal)
	}

	if callCount != 2 {
		t.Errorf("TestRelisterListPagination: expected 2 calls to lister for pagination, got %d", callCount)
	}
}

func TestRelisterListContextCancellation(t *testing.T) {
	blockingLister := func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
		select {
		case <-ctx.Done():
			return nil, "", context.Cause(ctx)
		case <-time.After(100 * time.Millisecond):
			pods := createTestPods(1)
			entries := createTestEntries(pods)
			return entries, "", nil
		}
	}

	r := &Relister{
		pagers: map[types.Retrieve]listPage{
			types.RTPod: blockingLister,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := r.List(ctx, types.RTPod)
	if err != nil {
		t.Fatalf("TestRelisterListContextCancellation: List() error = %v", err)
	}

	// Cancel context immediately
	cancel()

	// Read from channel - should get an error
	var gotError bool
	for response := range ch {
		if response.Err != nil {
			gotError = true
			if !errors.Is(response.Err, context.Canceled) {
				t.Errorf("TestRelisterListContextCancellation: expected context.Canceled error, got %v", response.Err)
			}
			break
		}
	}

	if !gotError {
		t.Error("TestRelisterListContextCancellation: expected error due to context cancellation")
	}
}

func TestRelisterListContextCancellationDuringIteration(t *testing.T) {
	pods := createTestPods(10)
	entries := createTestEntries(pods)

	slowLister := func(ctx context.Context, opts metav1.ListOptions) ([]data.Entry, string, error) {
		return entries, "", nil
	}

	r := &Relister{
		pagers: map[types.Retrieve]listPage{
			types.RTPod: slowLister,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := r.List(ctx, types.RTPod)
	if err != nil {
		t.Fatalf("TestRelisterListContextCancellationDuringIteration: List() error = %v", err)
	}

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
		t.Error("TestRelisterListContextCancellationDuringIteration: expected error due to context cancellation during iteration")
	}

	if entryCount == 0 {
		t.Error("TestRelisterListContextCancellationDuringIteration: should have read at least one entry before cancellation")
	}
}

func TestRelisterIntegrationWithFakeClientset(t *testing.T) {
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

	relister, err := New(clientset)
	if err != nil {
		t.Fatalf("TestRelisterIntegrationWithFakeClientset: New() error = %v", err)
	}

	ctx := context.Background()
	ch, err := relister.List(ctx, types.RTPod)
	if err != nil {
		t.Fatalf("TestRelisterIntegrationWithFakeClientset: List() error = %v", err)
	}

	var receivedEntries []data.Entry
	for response := range ch {
		if response.Err != nil {
			t.Fatalf("TestRelisterIntegrationWithFakeClientset: unexpected error from channel: %v", response.Err)
		}
		receivedEntries = append(receivedEntries, response.V)
	}

	if len(receivedEntries) != 2 {
		t.Errorf("TestRelisterIntegrationWithFakeClientset: got %d entries, want 2", len(receivedEntries))
	}

	// Verify entries have correct properties
	for i, entry := range receivedEntries {
		if entry.SourceType() != data.STWatchList {
			t.Errorf("TestRelisterIntegrationWithFakeClientset: entry[%d].SourceType() = %v, want %v", i, entry.SourceType(), data.STWatchList)
		}
		if entry.ChangeType() != data.CTSnapshot {
			t.Errorf("TestRelisterIntegrationWithFakeClientset: entry[%d].ChangeType() = %v, want %v", i, entry.ChangeType(), data.CTSnapshot)
		}
		if entry.ObjectType() != data.OTPod {
			t.Errorf("TestRelisterIntegrationWithFakeClientset: entry[%d].ObjectType() = %v, want %v", i, entry.ObjectType(), data.OTPod)
		}
	}
}

func TestRelisterChannelClosure(t *testing.T) {
	pods := createTestPods(1)
	entries := createTestEntries(pods)

	r := &Relister{
		pagers: map[types.Retrieve]listPage{
			types.RTPod: fakeListPage(entries, "", nil),
		},
	}

	ctx := context.Background()
	ch, err := r.List(ctx, types.RTPod)
	if err != nil {
		t.Fatalf("TestRelisterChannelClosure: List() error = %v", err)
	}

	// Read all entries
	entryCount := 0
	for response := range ch {
		if response.Err != nil {
			t.Fatalf("TestRelisterChannelClosure: unexpected error: %v", response.Err)
		}
		entryCount++
	}

	// Channel should be closed now
	select {
	case response, ok := <-ch:
		if ok {
			t.Errorf("TestRelisterChannelClosure: channel should be closed, but received: %v", response)
		}
	default:
		// This is expected - channel is closed
	}

	if entryCount != 1 {
		t.Errorf("TestRelisterChannelClosure: expected 1 entry, got %d", entryCount)
	}
}

// Benchmark tests
func BenchmarkRelisterList(b *testing.B) {
	pods := createTestPods(100)
	entries := createTestEntries(pods)

	r := &Relister{
		pagers: map[types.Retrieve]listPage{
			types.RTPod: fakeListPage(entries, "", nil),
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
		pagers: map[types.Retrieve]listPage{
			types.RTPod: paginatedLister,
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
