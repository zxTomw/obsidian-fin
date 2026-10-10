package errs_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"obsidian-fin/internal/errs"
)

func TestExitCodesAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{{nil, 0}, {errors.New("operational"), 1}, {errs.New(2, "usage"), 2}, {errs.New(3, "conflict"), 3}, {context.Canceled, 130}, {errs.Wrap(1, "canceled", context.Canceled), 130}} {
		if got := errs.ExitCode(tc.err); got != tc.code {
			t.Fatalf("code=%d want=%d", got, tc.code)
		}
	}
	for _, id := range []string{"access-sandbox-private", "public-sandbox-private", "link-production-private", "secret-private", "request\nprivate", "https://private.invalid"} {
		err := (&errs.APIError{Code: "ITEM_LOGIN_REQUIRED", RequestID: id}).Error()
		if strings.Contains(err, "private") {
			t.Fatal("unsafe error identifier leaked")
		}
	}
}
