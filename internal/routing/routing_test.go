package routing

import (
	"context"
	"testing"

	"github.com/Azure/tattler/internal/batching"

	"github.com/kylelemons/godebug/pretty"
)

func TestNew(t *testing.T) {
	t.Parallel()

	goodCh := make(chan batching.Batches)

	tests := []struct {
		name    string
		input   chan batching.Batches
		wantErr bool
	}{
		{
			name:    "Error: input is nil",
			wantErr: true,
		},
		{
			name:  "Success",
			input: goodCh,
		},
	}

	for _, test := range tests {
		b, err := New(context.Background(), test.input)
		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestNew(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestNew(%s): got err == %s, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		if b.input == nil {
			t.Errorf("TestNew(%s): should have set .input, but did not", test.name)
			continue
		}
		if b.routes == nil {
			t.Errorf("TestNew(%s): should have set .routes, but did not", test.name)
			continue
		}
		if b.log == nil {
			t.Errorf("TestNew(%s): should have set .log, but did not", test.name)
		}
	}

}

func TestRegister(t *testing.T) {
	t.Parallel()

	goodCh := make(chan batching.Batches)

	tests := []struct {
		name      string
		routeName string
		ch        chan batching.Batches
		started   bool
		wantErr   bool
	}{
		{
			name:      "Error: Started already",
			routeName: "route",
			ch:        goodCh,
			started:   true,
			wantErr:   true,
		},
		{
			name:    "Error: name is empty",
			ch:      goodCh,
			wantErr: true,
		},
		{
			name:      "Error: ch is nil",
			routeName: "route",
			wantErr:   true,
		},
		{
			name:      "Success",
			routeName: "route",
			ch:        goodCh,
		},
	}

	for _, test := range tests {
		b := &Batches{started: test.started}

		err := b.Register(context.Background(), test.routeName, test.ch)
		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestRegister(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestRegister(%s): got err == %s, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		if len(b.routes) != 1 {
			t.Errorf("TestRegister(%s): route was no added as expected", test.name)
		}
	}
}

func TestStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		b       *Batches
		wantErr bool
	}{
		{
			name:    "Error: No routes",
			b:       &Batches{},
			wantErr: true,
		},
		{
			name: "Success",
			b:    &Batches{routes: []route{route{out: make(chan batching.Batches, 1)}}},
		},
	}

	for _, test := range tests {
		err := test.b.Start(context.Background())
		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestStart(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestStart(%s): got err == %s, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		select {
		case <-test.b.routes[0].out:
			t.Errorf("TestStart(%s): <-test.b.routes[0].out succeeded, but should not have", test.name)
			continue
		default:
		}

		close(test.b.routes[0].out)

		<-test.b.routes[0].out
	}

}

func TestPush(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		route   route
		want    batching.Batches
		wantErr bool
	}{
		{
			name:    "Error: full channel",
			route:   route{name: "test", out: make(chan batching.Batches)},
			wantErr: true,
		},
		{
			name:  "Success",
			route: route{name: "test", out: make(chan batching.Batches, 1)},
			want:  batching.Batches{},
		},
	}

	for _, test := range tests {
		b := &Batches{}

		err := b.push(context.Background(), test.route, test.want)
		switch {
		case err == nil && test.wantErr:
			t.Errorf("TestPush(%s): got err == nil, want err != nil", test.name)
			continue
		case err != nil && !test.wantErr:
			t.Errorf("TestPush(%s): got err == %s, want err == nil", test.name, err)
			continue
		case err != nil:
			continue
		}

		if diff := pretty.Compare(test.want, <-test.route.out); diff != "" {
			t.Errorf("TestPush(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}
