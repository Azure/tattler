package tattler

import (
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/readers/apiserver/watchlist"
	"github.com/Azure/tattler/readers/apiserver/watchlist/types"
	"github.com/gostdlib/base/context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type fakeProcessor struct {
	receivedCh chan data.Entry
}

func newFakeProcessor() *fakeProcessor {
	return &fakeProcessor{
		receivedCh: make(chan data.Entry, 10),
	}
}

func (f *fakeProcessor) process(ctx context.Context, entryCh <-chan data.Entry) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-entryCh:
			if !ok {
				return
			}
			f.receivedCh <- entry
		}
	}
}

func TestEndToEnd(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	testPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			UID:       k8stypes.UID("test-uid-123"),
		},
	}

	clientset := fake.NewSimpleClientset()
	
	// Create a fake watcher that we control
	watcher := watch.NewFake()

	// Set up the fake clientset to return our watcher
	clientset.PrependWatchReactor("pods", func(action k8stesting.Action) (handled bool, ret watch.Interface, err error) {
		return true, watcher, nil
	})

	// Create the watchlist reader
	reader, err := watchlist.New(ctx, clientset, types.RTPod)
	if err != nil {
		t.Fatalf("TestEndToEnd: failed to create watchlist reader: %v", err)
	}

	// Create the input channel for tattler
	inputCh := make(chan data.Entry, 10)

	// Create the tattler runner using New()
	runner, err := New(ctx, inputCh)
	if err != nil {
		t.Fatalf("TestEndToEnd: failed to create tattler: %v", err)
	}

	// Add the reader to tattler
	err = runner.AddReader(ctx, reader)
	if err != nil {
		t.Fatalf("TestEndToEnd: failed to add reader: %v", err)
	}

	// Create a processor channel and register it
	processorCh := make(chan data.Entry, 10)
	err = runner.AddProcessor(ctx, "test-processor", processorCh)
	if err != nil {
		t.Fatalf("TestEndToEnd: failed to add processor: %v", err)
	}

	// Create and start the fake processor
	processor := newFakeProcessor()
	go processor.process(ctx, processorCh)

	// Start tattler
	err = runner.Start(ctx)
	if err != nil {
		t.Fatalf("TestEndToEnd: failed to start tattler: %v", err)
	}

	// Give the watcher time to start
	time.Sleep(100 * time.Millisecond)

	// Now send an event through the watcher
	watcher.Add(testPod)

	// Wait for the event to reach the processor
	select {
	case entry := <-processor.receivedCh:
		if entry.ObjectType() != data.OTPod {
			t.Errorf("TestEndToEnd: expected object type Pod, got %v", entry.ObjectType())
		}
		if entry.SourceType() != data.STWatchList {
			t.Errorf("TestEndToEnd: expected source type WatchList, got %v", entry.SourceType())
		}
		if entry.UID() != testPod.UID {
			t.Errorf("TestEndToEnd: expected UID %v, got %v", testPod.UID, entry.UID())
		}

		pod, err := entry.Pod()
		if err != nil {
			t.Errorf("TestEndToEnd: failed to get pod from entry: %v", err)
		}
		if pod.Name != testPod.Name {
			t.Errorf("TestEndToEnd: expected pod name %v, got %v", testPod.Name, pod.Name)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("TestEndToEnd: timeout waiting for event to reach processor")
	}
}