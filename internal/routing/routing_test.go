package routing

import (
	"testing"

	"github.com/Azure/tattler/data"
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
			b:    &Router{routes: []route{route{out: make(chan data.Entry, 1)}}},
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
		want    data.Entry
		wantErr bool
	}{
		{
			name:    "Error: full channel",
			route:   route{name: "test", out: make(chan data.Entry)},
			wantErr: true,
		},
		{
			name:  "Success",
			route: route{name: "test", out: make(chan data.Entry, 1)},
			want:  data.Entry{},
		},
	}

	for _, test := range tests {
		b := &Router{}

		err := b.push(t.Context(), test.route, test.want)
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
