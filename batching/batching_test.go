package batching

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/google/uuid"
	"github.com/gostdlib/concurrency/prim/wait"

	"github.com/kylelemons/godebug/pretty"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	_ "go.uber.org/automaxprocs"
)

// TestBatchingLimit tests that the batching limit is respected.
func TestBatchingLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := make(chan data.Entry, 1000)
	out := make(chan Batches, 1)
	finals := [][]data.Entry{}

	_, err := New(ctx, in, out, 3*time.Second)
	if err != nil {
		panic(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for entry := range out {
			if entry.Len() > 1000 {
				panic("batch size is too big")
			}
			for _, batch := range entry {
				l := make([]data.Entry, 0, len(batch.Data))
				for _, v := range batch.Data.Map() {
					l = append(l, v)
				}
				finals = append(finals, l)
			}
			entry.Recycle()
		}
	}()

	g := wait.Group{}
	g.Go(
		ctx,
		func(ctx context.Context) error {
			for i := 0; i < 1000000; i++ {
				pod := &corev1.Pod{
					ObjectMeta: v1.ObjectMeta{
						UID: types.UID(uuid.New().String()),
					},
				}
				in <- data.MustNewEntry(pod, data.STWatchList, data.CTAdd)
			}
			return nil
		},
	)
	g.Go(
		ctx,
		func(ctx context.Context) error {
			for i := 0; i < 150000; i++ {
				node := &corev1.Node{
					ObjectMeta: v1.ObjectMeta{
						UID: types.UID(uuid.New().String()),
					},
				}
				in <- data.MustNewEntry(node, data.STWatchList, data.CTAdd)
			}
			return nil
		},
	)

	g.Go(
		ctx,
		func(ctx context.Context) error {
			for i := 0; i < 1000; i++ {
				ns := &corev1.Namespace{
					ObjectMeta: v1.ObjectMeta{
						UID: types.UID(uuid.New().String()),
					},
				}
				in <- data.MustNewEntry(ns, data.STWatchList, data.CTAdd)
			}
			return nil
		},
	)

	if err := g.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	close(in)

	<-done

	for _, final := range finals {
		if len(final) > 1000 {
			t.Fatalf("batch size is too big: %d", len(final))
		}
	}
}

func TestHandleInput(t *testing.T) {
	t.Parallel()

	closedCh := make(chan data.Entry)
	close(closedCh)

	om := v1.ObjectMeta{
		UID: types.UID("test"),
	}

	batch99 := Batch{Data: map[types.UID]data.Entry{}}
	for i := 0; i < 99; i++ {
		om := v1.ObjectMeta{
			UID: types.UID("test"),
		}
		batch99.Data[types.UID(uuid.New().String())] = data.MustNewEntry(&corev1.Pod{ObjectMeta: om}, data.STInformer, data.CTAdd)
	}

	tests := []struct {
		name      string
		in        func() chan data.Entry
		tick      <-chan time.Time
		batchSize int
		current   Batches
		wantEmit  bool
		wantExit  bool
		wantErr   bool
	}{
		{
			name:     "Input channel is closed",
			in:       func() chan data.Entry { return closedCh },
			wantExit: true,
		},
		{
			name: "HandleData error",
			in: func() chan data.Entry {
				ch := make(chan data.Entry, 1)
				ch <- data.Entry{}
				return ch
			},
			wantErr: true,
		},
		{
			name: "Successful input",
			in: func() chan data.Entry {
				ch := make(chan data.Entry, 1)
				ch <- data.MustNewEntry(&corev1.Pod{ObjectMeta: om}, data.STInformer, data.CTAdd)
				return ch
			},
		},
		{
			name: "Successful tick but nothing to send",
			in:   func() chan data.Entry { return make(chan data.Entry) },
			tick: time.After(1 * time.Microsecond),
		},
		{
			name: "Successful tick and data to send",
			in:   func() chan data.Entry { return make(chan data.Entry) },
			tick: time.After(1 * time.Microsecond),
			current: Batches{data.STInformer: Batch{
				Data{
					types.UID(uuid.New().String()): data.MustNewEntry(&corev1.Pod{ObjectMeta: om}, data.STInformer, data.CTAdd),
				},
				time.Now(),
			}},
			wantEmit: true,
		},
		{
			name: "After 100 items",
			in: func() chan data.Entry {
				ch := make(chan data.Entry, 1)
				ch <- data.MustNewEntry(&corev1.Pod{ObjectMeta: om}, data.STInformer, data.CTAdd)
				return ch
			},
			batchSize: 100,
			tick:      time.After(1 * time.Hour),
			current:   Batches{data.STInformer: batch99},
			wantEmit:  true,
		},
	}

	for _, test := range tests {
		ctx := context.Background()
		if test.current == nil {
			test.current = getBatches()
		}
		b := &Batcher{
			in:        test.in(),
			batchSize: test.batchSize,
			current:   test.current,
		}
		var emitted bool
		emitter := func(_ context.Context) {
			emitted = true
		}
		b.emitter = emitter

		gotExit, gotErr := b.handleInput(ctx, test.tick)
		switch {
		case gotErr != nil && !test.wantErr:
			t.Errorf("TestHandleInput(%s): got err == %v, want err == nil", test.name, gotErr)
			continue
		case gotErr == nil && test.wantErr:
			t.Errorf("TestHandleInput(%s): got err == nil, want err != nil", test.name)
			continue
		case gotErr != nil:
			continue
		}

		if gotExit != test.wantExit {
			t.Errorf("TestHandleInput(%s): (exit value): got %v, want %v", test.name, gotExit, test.wantExit)
		}

		if emitted != test.wantEmit {
			t.Errorf("TestHandleInput(%s): (emitted value): got %v, want %v", test.name, emitted, test.wantEmit)
		}
	}
}

func TestEmit(t *testing.T) {
	t.Parallel()

	batches := Batches{
		data.STInformer: Batch{
			Data{
				"test": data.MustNewEntry(&corev1.Pod{}, data.STInformer, data.CTAdd),
			},
			time.Now(),
		},
	}

	b := &Batcher{
		out:     make(chan Batches, 1),
		current: batches,
	}

	b.emit(context.Background())

	select {
	case got := <-b.out:
		if diff := pretty.Compare(batches, got); diff != "" {
			t.Errorf("TestEmit(emitted data): -want/+got:\n%s", diff)
		}
		return
	default:
		t.Error("TestEmit: expected data on out channel")
	}

	if diff := pretty.Compare(b.current, Batches{}); diff != "" {
		t.Errorf("TestEmit(after emit): -want/+got:\n%s", diff)
	}
}

func TestHandleData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    data.Entry
		wantErr bool
	}{
		{
			name:    "Invalid data type",
			data:    data.Entry{},
			wantErr: true,
		},
		{
			name: "Valid data",
			data: data.MustNewEntry(&corev1.Pod{ObjectMeta: v1.ObjectMeta{UID: types.UID("test")}}, data.STInformer, data.CTAdd),
		},
		{
			name: "Valid delete event data",
			data: data.MustNewEntry(&corev1.Pod{ObjectMeta: v1.ObjectMeta{UID: types.UID("test")}}, data.STInformer, data.CTDelete),
		},
	}

	for _, test := range tests {
		b := &Batcher{
			current: make(Batches),
		}

		err := b.handleData(test.data)
		switch {
		case err != nil && !test.wantErr:
			t.Errorf("TestHandleData(%s): got error %v, want %v", test.name, err, test.wantErr)
			continue
		case err == nil && test.wantErr:
			t.Errorf("TestHandleData(%s): got %v, want error", test.name, err)
			continue
		case err != nil:
			continue
		}

		if diff := pretty.Compare(test.data, b.current[test.data.SourceType()].Data[test.data.UID()]); diff != "" {
			t.Errorf("TestHandleData(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}

// This is mostly testing that we don't put the wrong data in the wrong pool.
func TestRecycle(t *testing.T) {
	t.Parallel()

	batches := Batches{
		data.STInformer: Batch{
			Data{
				"test":  data.MustNewEntry(&corev1.Pod{}, data.STInformer, data.CTAdd),
				"test2": data.MustNewEntry(&corev1.Pod{}, data.STInformer, data.CTAdd),
			},
			time.Now(),
		},
	}

	batches.Recycle()

	// If we put the wrong data in the wrong pool, these will panic.
	bs := getBatches()
	if diff := pretty.Compare(Batches{}, bs); diff != "" {
		t.Errorf("TestRecycle: -want/+got:\n%s", diff)
	}
	b := getBatch()
	if diff := pretty.Compare(Batch{}, b); diff != "" {
		t.Errorf("TestRecycle: -want/+got:\n%s", diff)
	}
}

func TestAll(t *testing.T) {
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
	meta1 := metav1.ObjectMeta{
		Name:              "a",
		ManagedFields:     managedFields,
		CreationTimestamp: creationTimestamp,
	}
	meta2 := metav1.ObjectMeta{
		Name:              "b",
		ManagedFields:     managedFields,
		CreationTimestamp: creationTimestamp,
	}

	tests := []struct {
		name    string
		batches Batches
		want    []data.Entry
	}{
		{
			name: "one entry in one batch",
			batches: Batches{
				data.STInformer: Batch{
					Data{
						"test": data.MustNewEntry(&corev1.Pod{ObjectMeta: meta1}, data.STInformer, data.CTAdd),
					},
					time.Now(),
				},
			},
			want: []data.Entry{data.MustNewEntry(&corev1.Pod{ObjectMeta: meta1}, data.STInformer, data.CTAdd)},
		},
		{
			name: "multiple entries in one batch",
			batches: Batches{
				data.STInformer: Batch{
					Data{
						"test":  data.MustNewEntry(&corev1.Pod{ObjectMeta: meta1}, data.STInformer, data.CTAdd),
						"test2": data.MustNewEntry(&corev1.Pod{ObjectMeta: meta2}, data.STInformer, data.CTUpdate),
					},
					time.Now(),
				},
			},
			want: []data.Entry{
				data.MustNewEntry(&corev1.Pod{ObjectMeta: meta1}, data.STInformer, data.CTAdd),
				data.MustNewEntry(&corev1.Pod{ObjectMeta: meta2}, data.STInformer, data.CTUpdate),
			},
		},
	}

	for _, test := range tests {
		entries := []data.Entry{}
		for d := range test.batches.All() {
			entries = append(entries, d)
		}

		sortPod := func(i, j int) bool {
			pod1, _ := entries[i].Pod()
			pod2, _ := entries[j].Pod()
			if pod1.Name < pod2.Name {
				return true
			}
			return false
		}

		sort.Slice(
			entries,
			sortPod,
		)
		sort.Slice(
			test.want,
			sortPod,
		)

		if diff := pretty.Compare(test.want, entries); diff != "" {
			t.Errorf("TestAll: .current: -want/+got:\n%s", diff)
		}
	}
}

func TestGetBatches(t *testing.T) {
	t.Parallel()

	b := getBatches()
	if diff := pretty.Compare(Batches{}, b); diff != "" {
		t.Errorf("TestGetBatches: -want/+got:\n%s", diff)
	}
}

func TestGetBatch(t *testing.T) {
	t.Parallel()

	b := getBatch()
	if diff := pretty.Compare(Batch{}, b); diff != "" {
		t.Errorf("TestGetBatch: -want/+got:\n%s", diff)
	}
}
