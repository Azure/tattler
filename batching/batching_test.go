package batching

import (
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/google/uuid"

	"github.com/kylelemons/godebug/pretty"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestHandleInput(t *testing.T) {
	t.Parallel()

	closedCh := make(chan data.Entry)
	close(closedCh)

	om := v1.ObjectMeta{
		UID: types.UID("test"),
	}

	batch99 := Batch{}
	for i := 0; i < 99; i++ {
		om := v1.ObjectMeta{
			UID: types.UID("test"),
		}
		batch99[types.UID(uuid.New().String())] = data.MustNewEntry(&corev1.Pod{ObjectMeta: om}, data.STInformer, data.CTAdd)
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
				types.UID(uuid.New().String()): data.MustNewEntry(&corev1.Pod{ObjectMeta: om}, data.STInformer, data.CTAdd),
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
		if test.current == nil {
			test.current = getBatches()
		}
		b := &Batcher{
			in:        test.in(),
			batchSize: test.batchSize,
			current:   test.current,
		}
		var emitted bool
		emitter := func() {
			emitted = true
		}
		b.emitter = emitter

		gotExit, gotErr := b.handleInput(test.tick)
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
			"test": data.MustNewEntry(&corev1.Pod{}, data.STInformer, data.CTAdd),
		},
	}

	b := &Batcher{
		out:     make(chan Batches, 1),
		current: batches,
	}

	b.emit()

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
		t.Errorf("TestEmit(after emit): .current: -want/+got:\n%s", diff)
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

		if diff := pretty.Compare(test.data, b.current[test.data.SourceType()][test.data.UID()]); diff != "" {
			t.Errorf("TestHandleData(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}

// This is mostly testing that we don't put the wrong data in the wrong pool.
func TestRecycle(t *testing.T) {
	t.Parallel()

	batches := Batches{
		data.STInformer: Batch{
			"test":  data.MustNewEntry(&corev1.Pod{}, data.STInformer, data.CTAdd),
			"test2": data.MustNewEntry(&corev1.Pod{}, data.STInformer, data.CTAdd),
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
