package sched

import (
	"errors"
	"fmt"

	"github.com/fables-for-robots/jobs-iroh/api"
)

// Error is a scheduler error carrying an api error code, so the protocol
// layer (serve) can map failures onto api.Error frames without string
// matching. Every error returned by the public Sched methods either is an
// *Error or maps to api.CodeInternal via ErrorCode.
type Error struct {
	Code string // one of the api.Code* constants
	Text string
}

func (e *Error) Error() string { return e.Text }

// ErrorCode extracts the api error code from err, defaulting to
// api.CodeInternal for anything the scheduler did not classify.
func ErrorCode(err error) string {
	var se *Error
	if errors.As(err, &se) {
		return se.Code
	}
	return api.CodeInternal
}

func badRequest(format string, args ...any) error {
	return &Error{Code: api.CodeBadRequest, Text: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &Error{Code: api.CodeNotFound, Text: fmt.Sprintf(format, args...)}
}

func incomplete(format string, args ...any) error {
	return &Error{Code: api.CodeIncomplete, Text: fmt.Sprintf(format, args...)}
}

func internal(format string, args ...any) error {
	return &Error{Code: api.CodeInternal, Text: fmt.Sprintf(format, args...)}
}
