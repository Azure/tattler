package watchlist

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Azure/retry/exponential"
	"github.com/Azure/tattler/data"
)

type fakeReader struct {
	closeCalled  bool
	setOutCalled bool
	runErr       error
}

func (f *fakeReader) Close(ctx context.Context) error {
	f.closeCalled = true
	return nil
}

func (f *fakeReader) SetOut(ctx context.Context, out chan data.Entry) error {
	f.setOutCalled = true
	return nil
}

func (f *fakeReader) Run(ctx context.Context) error {
	if f.runErr != nil {
		return f.runErr
	}
	return nil
}

func (f *fakeReader) Logger() *slog.Logger {
	return nil
}

func (f *fakeReader) Relist() time.Duration {
	return 1 * time.Hour
}

func TestClose(t *testing.T) {
	t.Parallel()

	f := &fakeReader{}

	r := &Reader{
		r:       f,
		ch:      make(chan data.Entry),
		closeCh: make(chan struct{}),
	}

	err := r.Close(context.Background())
	if err != nil {
		t.Errorf("TestClose: unexpected error: %v", err)
	}
	if !f.closeCalled {
		t.Error("TestClose: expected underlying reader.Close to be called")
	}
	select {
	case <-r.ch:
	default:
		t.Error("TestClose: expected output channel to be closed")
	}
}

func TestSetOut(t *testing.T) {
	t.Parallel()

	f := &fakeReader{}

	r := &Reader{
		r: f,
	}

	out := make(chan data.Entry, 1)
	err := r.SetOut(context.Background(), out)
	if err != nil {
		t.Errorf("TestSetOut: unexpected error: %v", err)
	}
	if !f.setOutCalled {
		t.Error("TestSetOut: expected underlying reader.SetOut to be called")
	}
	if r.ch != out {
		t.Error("TestSetOut: expected output channel to be set")
	}
}

func TestRun(t *testing.T) {
	t.Parallel()

	handleSwitchCalled := false
	hs := func() {
		handleSwitchCalled = true
	}

	f := &fakeReader{}

	r := &Reader{
		r:                      f,
		testHandleClientSwitch: hs,
	}

	err := r.Run(context.Background())
	if err != nil {
		t.Errorf("TestRun: unexpected error: %v", err)
	}
	if !handleSwitchCalled {
		t.Errorf("TestRun: expected handleClientSwitch to be called")
	}
}

func TestSwitchWait(t *testing.T) {
	t.Parallel()

	closed := make(chan struct{})
	close(closed)

	tests := []struct {
		name    string
		timer   *time.Timer
		closeCh chan struct{}
	}{
		{
			name:    "timer expired",
			timer:   time.NewTimer(1 * time.Millisecond),
			closeCh: make(chan struct{}),
		},
		{
			name:    "close channel closed",
			timer:   time.NewTimer(1 * time.Hour),
			closeCh: closed,
		},
	}

	for _, test := range tests {
		switched := false
		r := &Reader{
			closeCh: test.closeCh,
			testClientSwitchRetry: func() {
				switched = true
			},
			r: &fakeReader{},
		}

		r.switchWait(test.timer)

		if test.closeCh != closed {
			if !switched {
				t.Errorf("TestSwitchWait(%s): expected client switch to be called", test.name)
			}
		} else {
			if switched {
				t.Errorf("TestSwitchWait(%s): expected client switch to not be called", test.name)
			}
		}
	}
}

func TestClientSwitch(t *testing.T) {
	t.Parallel()

	closed := make(chan struct{})
	close(closed)

	tests := []struct {
		name      string
		closeCh   chan struct{}
		newReader func() (watchReader, error)
		wantErr   bool
	}{
		{
			name:    "Close() has been called",
			closeCh: closed,
		},
		{
			name:    "new reader returns error",
			closeCh: make(chan struct{}),
			newReader: func() (watchReader, error) {
				return nil, errors.New("error")
			},
			wantErr: true,
		},
		{
			name:    "reader.Run() returns error",
			closeCh: make(chan struct{}),
			newReader: func() (watchReader, error) {
				return &fakeReader{
					runErr: errors.New("error"),
				}, nil
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		oldReader := &fakeReader{}
		r := &Reader{
			closeCh:   test.closeCh,
			newReader: test.newReader,
			r:         oldReader,
		}

		err := r.clientSwitch(context.Background(), exponential.Record{})
		switch {
		case test.wantErr && err == nil:
			t.Errorf("TestClientSwitch(%s): expected error", test.name)
			continue
		case !test.wantErr && err != nil:
			t.Errorf("TestClientSwitch(%s): unexpected error: %v", test.name, err)
			continue
		case err != nil:
			continue
		}

		// If this is the test for context cancellation, we do not need to check anything.
		select {
		case <-test.closeCh:
			continue
		default:
		}

		if !oldReader.closeCalled {
			t.Errorf("TestClientSwitch(%s): expected underlying reader.Close to be called", test.name)
		}

		if !r.r.(*fakeReader).setOutCalled {
			t.Errorf("TestClientSwitch(%s): expected underlying reader.SetOut to be called", test.name)
		}
	}
}
