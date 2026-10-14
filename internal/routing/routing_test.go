package routing

import (
	"testing"

	"github.com/Azure/tattler/data"
	"github.com/gostdlib/base/values/chans"
	"github.com/kylelemons/godebug/pretty"
)

func TestNew(t *testing.T) {
	t.Parallel()

	goodCh := make(chan data.Entry)

	tests := []struct {
		name    string
		input   chan data.Entry
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
		b, err := New(t.Context(), test.input)
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

		if b.in == nil {
			t.Errorf("TestNew(%s): should have set .input, but did not", test.name)
			continue
		}
		if b.routes == nil {
			t.Errorf("TestNew(%s): should have set .routes, but did not", test.name)
			continue
		}
	}

}

func TestRegister(t *testing.T) {
	t.Parallel()

	goodCh := make(chan data.Entry)

	tests := []struct {
		name      string
		routeName string
		ch        chan data.Entry
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
		b := &Router{started: test.started}

		err := b.Register(t.Context(), test.routeName, test.ch)
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
			t.Errorf("TestRegister(%s): route was not added as expected", test.name)
		}
	}
}

func TestStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		b       *Router
		wantErr bool
	}{
		{
			name:    "Error: No routes",
			b:       &Router{},
			wantErr: true,
		},
		{
			name: "Success",
			b:    &Router{routes: []route{newRoute("route", make(chan data.Entry, 1))}},
		},
	}

	for _, test := range tests {
		err := test.b.Start(t.Context())
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

		if !test.b.started {
			t.Errorf("TestStart(%s): got started == false, want started == true", test.name)
		}

		if _, ok, _ := chans.TryGet(test.b.routes[0].out); ok {
			t.Errorf("TestStart(%s): <-test.b.routes[0].out succeeded, but should not have", test.name)
			continue
		}

		close(test.b.routes[0].out)

		<-test.b.routes[0].out
	}

}

func TestPush(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  chan data.Entry
		// priorDrops is how many entries the route had already dropped.
		priorDrops  uint64
		want        data.Entry
		wantDropped uint64
	}{
		{
			name:        "Success: a full channel drops the entry and counts it",
			out:         make(chan data.Entry),
			wantDropped: 1,
		},
		{
			name:        "Success: a drop adds to the count of earlier drops",
			out:         make(chan data.Entry),
			priorDrops:  3,
			wantDropped: 4,
		},
		{
			name: "Success: a channel with room gets the entry",
			out:  make(chan data.Entry, 1),
			want: data.Entry{},
		},
	}

	for _, test := range tests {
		b := &Router{}
		r := newRoute("test", test.out)
		r.dropped.Store(test.priorDrops)

		dropped := b.push(r, test.want)
		if dropped != test.wantDropped {
			t.Errorf("TestPush(%s): got dropped == %d, want %d", test.name, dropped, test.wantDropped)
			continue
		}
		if dropped != 0 {
			continue
		}

		if diff := pretty.Compare(test.want, <-test.out); diff != "" {
			t.Errorf("TestPush(%s): -want/+got:\n%s", test.name, diff)
		}
	}
}
