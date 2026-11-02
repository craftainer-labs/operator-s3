package v1alpha1

import (
	"net/url"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
)

// redacted stands in for a URL's value in an error: a URL can carry
// credentials (https://user:password@host), and an admission error is shown
// to the user and written to logs.
const redacted = "<redacted>"

// invalid turns a list of field errors into the 422 the API server returns,
// or nil for an empty list.
func invalid(kind, name string, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(schema.GroupKind{Group: s3v1alpha1.GroupVersion.Group, Kind: kind}, name, errs)
}

// validURL reports whether raw parses as a URL with one of the schemes and a
// host.
func validURL(raw string, schemes ...string) bool {
	u, err := url.Parse(raw)
	return err == nil && slices.Contains(schemes, u.Scheme) && u.Hostname() != ""
}
