package bmc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
)

// ErrorKind classifies BMC failures. The classification drives job-level
// retryability and gives clients a machine-readable code (docs/07-bmc.md §1).
type ErrorKind string

const (
	// KindUnreachable: network-level failure (timeout, conn refused, DNS).
	// Retryable: the target may come back.
	KindUnreachable ErrorKind = "BMC_UNREACHABLE"
	// KindAuthFailed: credential rejected. Not retryable as-is.
	KindAuthFailed ErrorKind = "BMC_AUTH_FAILED"
	// KindUnsupported: capability or protocol missing on this BMC.
	KindUnsupported ErrorKind = "BMC_UNSUPPORTED"
	// KindProtocolError: vendor firmware misbehaved against the standard.
	// Retryable: firmware glitches are frequently transient.
	KindProtocolError ErrorKind = "BMC_PROTOCOL_ERROR"
)

// Error is the BMC-layer error. Code() returns the machine-readable code.
type Error struct {
	Kind   ErrorKind
	Op     string
	Detail string
	Err    error
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Op + ": " + string(e.Kind) + ": " + e.Detail
	}
	return e.Op + ": " + string(e.Kind)
}

func (e *Error) Unwrap() error { return e.Err }

// Code is the registry error code for this failure.
func (e *Error) Code() string { return string(e.Kind) }

// Retryable reports whether the same call may succeed on retry.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindUnreachable, KindProtocolError:
		return true
	default:
		return false
	}
}

func opErr(op string, kind ErrorKind, err error, detail string) *Error {
	return &Error{Kind: kind, Op: op, Detail: detail, Err: err}
}

// ErrUnsupported is a sentinel callers can test with errors.Is.
var ErrUnsupported = errors.New("operation not supported by this BMC or driver")

// Classify converts an arbitrary error from a protocol exchange into a typed
// *Error. Unrecognized errors land on KindProtocolError only when they clearly
// originate from a BMC exchange; transport-level signals are honored first.
func Classify(op string, err error) *Error {
	if err == nil {
		return nil
	}
	var bmcErr *Error
	if errors.As(err, &bmcErr) {
		return bmcErr
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return opErr(op, KindUnreachable, err, "deadline exceeded")
	}
	if errors.Is(err, ErrUnsupported) {
		return opErr(op, KindUnsupported, err, "")
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return opErr(op, KindUnreachable, err, "timeout")
		}
		return opErr(op, KindUnreachable, err, netErr.Error())
	}
	if errors.Is(err, ErrAuthFailedSentinel) {
		return opErr(op, KindAuthFailed, err, "")
	}

	// HTTP status signals embedded by transport clients.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "401") || strings.Contains(strings.ToLower(msg), "unauthorized"):
		return opErr(op, KindAuthFailed, err, "")
	case strings.Contains(msg, "404") || strings.Contains(strings.ToLower(msg), "not found"):
		return opErr(op, KindProtocolError, err, "resource missing (http 404)")
	default:
		return opErr(op, KindUnreachable, err, msg)
	}
}

// ErrAuthFailedSentinel marks credential failures raised by protocol clients
// (which often only see a non-200 status without structured errors).
var ErrAuthFailedSentinel = errors.New("bmc: authentication failed")

// HttpStatusKind maps an HTTP status from a Redfish exchange to an error
// kind (nil when the status is not an error).
func HttpStatusKind(op string, status int, body string) *Error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return opErr(op, KindAuthFailed, nil, http.StatusText(status))
	case status == http.StatusNotFound:
		return opErr(op, KindProtocolError, nil, "http 404: "+FirstLine(body))
	case status == http.StatusServiceUnavailable || status == http.StatusBadGateway || status == http.StatusGatewayTimeout:
		return opErr(op, KindUnreachable, nil, http.StatusText(status))
	case status >= 400:
		return opErr(op, KindProtocolError, nil, FirstLine(body))
	default:
		return nil
	}
}

// FirstLine returns the first line of a response body, bounded — for error
// details surfaced to clients.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 500 {
		s = s[:500]
	}
	return strings.TrimSpace(s)
}
