// Package apierr classifies errors returned by the Kubernetes APIServer for the retry loops in the watchlist reader.
package apierr

import (
	"fmt"

	"github.com/gostdlib/base/retry/exponential"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Permanent wraps err with exponential.ErrPermanent if retrying the call cannot succeed: the object is not found,
// the caller is forbidden or unauthorized, or the request is invalid. Any other error, including nil, is returned
// unchanged.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) {
		return fmt.Errorf("%w: %w", err, exponential.ErrPermanent)
	}
	return err
}
