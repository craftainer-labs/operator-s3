package s3client

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
)

// The wrappers of another backend client carry a category as their text, and
// the cause only through errors.Is.
func TestExportedFailureWrappers(t *testing.T) {
	cause := errors.New("dial tcp 10.1.2.3:443: secret detail")
	for name, tc := range map[string]struct {
		err      error
		category error
	}{
		"unreachable": {Unreachable(cause), ErrConnFailed},
		"policy":      {Unreachable(egress.ErrBlocked), ErrConnPolicy},
		"timeout":     {Unreachable(context.DeadlineExceeded), ErrConnTimeout},
		"auth":        {AuthFailed(cause), ErrConnAuth},
	} {
		if !errors.Is(tc.err, lifecycle.ErrUnreachable) || !errors.Is(tc.err, tc.category) {
			t.Errorf("%s: %v", name, tc.err)
		}
		if strings.Contains(tc.err.Error(), "secret detail") || strings.Contains(tc.err.Error(), "10.1.2.3") {
			t.Errorf("%s leaks the cause: %v", name, tc.err)
		}
	}
	if !errors.Is(Unreachable(cause), cause) {
		t.Error("the cause is lost")
	}
}

func TestLoggableCode(t *testing.T) {
	if LoggableCode("NoSuchBucket") != "NoSuchBucket" || LoggableCode("a b") != invalidCode {
		t.Error("loggable code")
	}
}
