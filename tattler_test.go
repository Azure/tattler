package tattler

import (
	"context"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/Azure/tattler/batching"
	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/internal/routing"
)

type fakeReader struct {
	setoutErr error
	runErr    error
	runCalled bool
}

func (f *fakeReader) SetOut(context.Context, chan data.Entry) error {
	return f.setoutErr
}

func (f *fakeReader) Run(context.Context) error {
	f.runCalled = true
	return f.runErr
}

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		in            chan data.Entry
		batchTimespan time.Duration
		wantErr       bool
	}{
		{
			name:          "Error: nil input",
			batchTimespan: time.Second,
			wantErr:       true,
		},
		{
			name:    "Error: 0 batchTimespan",
			in:      make(chan data.Entry, 1),
			wantErr: true,
		},
		{
			name:          "Success",
			in:            make(chan data.Entry, 1),
			batchTimespan: time.Second,
		},
	}

	for _, test := range tests {
		r, err := New(context.Background(), test.in, test.batchTimespan)
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

		if r.secrets == nil {
			t.Errorf("TestNew(%s): got tat.secrets == nil, want tat.secrets != nil", test.name)
		}
		if r.batcher == nil {
			t.Errorf("TestNew(%s): got tat.batcher == nil, want tat.batcher != nil", test.name)
		}
		if r.router == nil {
			t.Errorf("TestNew(%s): got tat.router == nil, want tat.router != nil", test.name)
		}
	}

}

func TestAddReader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		reader  *fakeReader
		started bool
		wantErr bool
	}{
		{
			name:    "Error: SetOut error",
			reader:  &fakeReader{setoutErr: errors.New("error")},
			wantErr: true,
		},
		{
			name:    "Error: Run error",
			reader:  &fakeReader{runErr: errors.New("error")},
			started: true,
			wantErr: true,
		},

		{
			name:    "Success, already started so run is called",
			reader:  &fakeReader{},
			started: true,
		},

		{
			name:   "Success, not started so run is not called",
			reader: &fakeReader{},
		},
	}

	for _, test := range tests {
		log.Println("test: ", test)
		r := &Runner{
			started: test.started,
		}

		err := r.AddReader(context.Background(), test.reader)
		log.Println("err: ", err)
		switch {
		case test.wantErr && err == nil:
			t.Errorf("AddReader(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("AddReader(%s): got err == %v, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		if len(r.readers) != 1 {
			t.Errorf("AddReader(%s): got len(r.readers) == %d, want len(r.readers) == 1", test.name, len(r.readers))
		}

		if test.started {
			if !test.reader.runCalled {
				t.Errorf("AddReader(%s): got reader.runCalled == false, want reader.runCalled == true", test.name)
			}
		} else {
			if test.reader.runCalled {
				t.Errorf("AddReader(%s): got reader.runCalled == true, want reader.runCalled == false", test.name)
			}
		}
	}

}

func TestAddProcessor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      chan batching.Batches
		started bool
		wantErr bool
	}{
		{
			name:    "Error: nil in",
			wantErr: true,
		},
		{
			name:    "Error: already started",
			in:      make(chan batching.Batches, 1),
			started: true,
			wantErr: true,
		},
		{
			name: "Success",
			in:   make(chan batching.Batches, 1),
		},
	}

	for _, test := range tests {
		router, err := routing.New(context.Background(), make(chan batching.Batches))
		if err != nil {
			panic(err)
		}

		r := &Runner{
			router:  router,
			started: test.started,
		}

		err = r.AddProcessor(context.Background(), test.name, test.in)
		switch {
		case test.wantErr && err == nil:
			t.Errorf("AddProcessor(%s): got err == nil, want err != nil", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("AddProcessor(%s): got err == %v, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		if !r.router.Exists(test.name) {
			t.Errorf("AddProcessor(%s): got len(r.router.Processors) == %d, want len(r.router.Processors) == 1", test.name, 1)
		}
	}
}
