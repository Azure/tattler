package tattler

import (
	"testing"
	"time"

	"github.com/Azure/tattler/data"
	"github.com/Azure/tattler/internal/routing"
	"github.com/gostdlib/base/context"
	"github.com/gostdlib/base/errors"
	"github.com/gostdlib/base/values/chans"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
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
		r := &Runner{
			started: test.started,
		}

		err := r.AddReader(t.Context(), test.reader)
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
		in      chan data.Entry
		started bool
		wantErr bool
	}{
		{
			name:    "Error: nil in",
			wantErr: true,
		},
		{
			name:    "Error: already started",
			in:      make(chan data.Entry, 1),
			started: true,
			wantErr: true,
		},
		{
			name: "Success",
			in:   make(chan data.Entry, 1),
		},
	}

	for _, test := range tests {
		router, err := routing.New(t.Context(), make(chan data.Entry))
		if err != nil {
			panic(err)
		}

		r := &Runner{
			router:  router,
			started: test.started,
		}

		err = r.AddProcessor(t.Context(), test.name, test.in)
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

func TestNew(t *testing.T) {
	t.Parallel()

	const redactedValue = "REDACTED"

	tests := []struct {
		name    string
		options []Option
	}{
		{
			name: "Success: without preprocessors the processor gets one scrubbed entry and closing in shuts down cleanly",
		},
		{
			name:    "Success: with a preprocessor the processor gets one scrubbed entry and closing in shuts down cleanly",
			options: []Option{WithPreProcessor(func(context.Context, data.Entry) error { return nil })},
		},
	}

	for _, test := range tests {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		in := make(chan data.Entry, 1)
		r, err := New(ctx, in, test.options...)
		if err != nil {
			t.Errorf("TestNew(%s): got err == %s, want err == nil", test.name, err)
			continue
		}
		out := make(chan data.Entry, 10)
		if err := r.AddProcessor(ctx, "processor", out); err != nil {
			t.Errorf("TestNew(%s): AddProcessor() got err == %s, want err == nil", test.name, err)
			continue
		}
		if err := r.Start(ctx); err != nil {
			t.Errorf("TestNew(%s): Start() got err == %s, want err == nil", test.name, err)
			continue
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "default", UID: k8stypes.UID("uid")},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Env: []corev1.EnvVar{{Name: "PASSWORD", Value: "hunter2"}}}},
			},
		}
		in <- data.MustNewEntry(pod, data.STWatchList, data.CTAdd)
		// Closing in is the documented way to stop the pipeline. Before safety had its own output channel this
		// closed in a second time and panicked.
		close(in)

		var got []data.Entry
		for {
			e, res := chans.Get(ctx, out)
			if res != chans.ResultOK {
				if res != chans.ResultClosed {
					t.Errorf("TestNew(%s): got chans.Get() result %v, want %v", test.name, res, chans.ResultClosed)
				}
				break
			}
			got = append(got, e)
		}

		if len(got) != 1 {
			t.Errorf("TestNew(%s): got %d entries at the processor, want 1", test.name, len(got))
			continue
		}
		gotPod, err := got[0].Pod()
		if err != nil {
			t.Errorf("TestNew(%s): got err == %s, want err == nil", test.name, err)
			continue
		}
		if v := gotPod.Spec.Containers[0].Env[0].Value; v != redactedValue {
			t.Errorf("TestNew(%s): got env value %q at the processor, want %q", test.name, v, redactedValue)
		}
	}
}
