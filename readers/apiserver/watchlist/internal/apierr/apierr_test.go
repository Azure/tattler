package apierr

import (
	"testing"

	"github.com/gostdlib/base/errors"
	"github.com/gostdlib/base/retry/exponential"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestPermanent(t *testing.T) {
	t.Parallel()

	gr := schema.GroupResource{Resource: "pods"}

	tests := []struct {
		name          string
		err           error
		wantPermanent bool
	}{
		{name: "Success: nil stays nil", err: nil},
		{name: "Success: not found is permanent", err: apierrors.NewNotFound(gr, "name"), wantPermanent: true},
		{name: "Success: forbidden is permanent", err: apierrors.NewForbidden(gr, "name", errors.New("rbac")), wantPermanent: true},
		{name: "Success: unauthorized is permanent", err: apierrors.NewUnauthorized("no token"), wantPermanent: true},
		{
			name:          "Success: invalid is permanent",
			err:           apierrors.NewInvalid(schema.GroupKind{Kind: "Pod"}, "name", field.ErrorList{field.Required(field.NewPath("spec"), "")}),
			wantPermanent: true,
		},
		{name: "Success: server timeout is retryable", err: apierrors.NewServerTimeout(gr, "list", 1)},
		{name: "Success: too many requests is retryable", err: apierrors.NewTooManyRequests("slow down", 1)},
		{name: "Success: a non APIServer error is retryable", err: errors.New("connection reset")},
	}

	for _, test := range tests {
		got := Permanent(test.err)

		if test.err == nil {
			if got != nil {
				t.Errorf("TestPermanent(%s): got err == %s, want err == nil", test.name, got)
			}
			continue
		}
		// exponential.Backoff.Retry stops retrying on exponential.ErrPermanent, which is the reason this classification
		// exists (relist.retryableList, configmap.retryAPIServer).
		if gotPermanent := errors.Is(got, exponential.ErrPermanent); gotPermanent != test.wantPermanent {
			t.Errorf("TestPermanent(%s): got errors.Is(err, exponential.ErrPermanent) == %v, want %v", test.name, gotPermanent, test.wantPermanent)
		}
		// Callers still need the APIServer error itself, for example to check apierrors.IsNotFound.
		if !errors.Is(got, test.err) {
			t.Errorf("TestPermanent(%s): got err that does not wrap the original error", test.name)
		}
	}
}
