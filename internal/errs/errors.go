// Package errs defines public, sanitized errors and CLI exit codes.
package errs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

type Error struct {
	Code    int
	Message string
	Cause   error
}

func (e *Error) Error() string           { return e.Message }
func (e *Error) Unwrap() error           { return e.Cause }
func New(code int, message string) error { return &Error{Code: code, Message: message} }
func Wrap(code int, message string, cause error) error {
	return &Error{Code: code, Message: message, Cause: cause}
}
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 1
}

// APIError excludes provider messages, URLs, request/response bodies and tokens.
type APIError struct {
	Code, RequestID string
	Status          int
}

var safeIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

func SafeIdentifier(s string) string {
	if safeIdentifier.MatchString(s) && !strings.Contains(s, "access-") && !strings.Contains(s, "public-") && !strings.Contains(s, "link-") && !strings.Contains(s, "secret-") {
		return s
	}
	return "unknown"
}
func (e *APIError) Error() string {
	remedy := "rerun the command"
	switch e.Code {
	case "ITEM_LOGIN_REQUIRED", "ITEM_LOCKED", "USER_PERMISSION_REVOKED":
		remedy = "run obsidian-fin connect --update"
	case "INVALID_ACCESS_TOKEN", "ITEM_NOT_FOUND":
		remedy = "verify the selected environment and connection; replacing revoked Items is outside v1"
	case "INVALID_API_KEYS", "INVALID_CLIENT_ID", "INVALID_SECRET":
		remedy = "verify the Plaid client ID and its Keychain secret for this environment"
	}
	return fmt.Sprintf("Plaid request failed (%s; request ID %s); %s", SafeIdentifier(e.Code), SafeIdentifier(e.RequestID), remedy)
}
func IsAPI(err error, code string) bool { var e *APIError; return errors.As(err, &e) && e.Code == code }
