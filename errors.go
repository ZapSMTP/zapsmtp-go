package zapsmtp

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

// Error is every error of the API, and a request that got no answer at all
// (StatusCode 0: a network failure or a timeout, with the cause in Unwrap).
// An argument the SDK refuses before sending (an empty id) is an Error of
// kind ErrValidation with StatusCode 0 and Code "invalid_parameter".
//
// Check its kind with errors.Is against ErrNotFound and the other sentinels, or
// with IsNotFound and the other helpers; read its fields with errors.As:
//
//	var apiErr *zapsmtp.Error
//	if errors.As(err, &apiErr) {
//		log.Printf("%d %s (request %s)", apiErr.StatusCode, apiErr.Code, apiErr.RequestID)
//	}
type Error struct {
	// StatusCode is the HTTP status; 0 when there was no answer.
	StatusCode int
	// Code is the API's error code, such as "invalid_parameter".
	Code string
	// Message says what went wrong.
	Message string
	// Type is the error type, such as "invalid_request_error".
	Type string
	// RequestID is the X-Request-Id header: quote it to support.
	RequestID string
	// Header holds the answer's headers (nil without an answer).
	Header http.Header
	// Body is the answer's body as sent.
	Body []byte

	cause error
	local error // the kind of an error found before sending
}

// Kinds of Error, for errors.Is.
var (
	ErrAuthentication = errors.New("zapsmtp: authentication failed")     // 401
	ErrPermission     = errors.New("zapsmtp: permission denied")         // 403
	ErrNotFound       = errors.New("zapsmtp: not found")                 // 404
	ErrConflict       = errors.New("zapsmtp: conflict")                  // 409
	ErrValidation     = errors.New("zapsmtp: invalid request")           // 400, 413, 422
	ErrRateLimit      = errors.New("zapsmtp: rate or quota limit")       // 429
	ErrServer         = errors.New("zapsmtp: server error")              // 5xx
	ErrConnection     = errors.New("zapsmtp: no answer from the server") // network failure or timeout
)

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("zapsmtp: ")
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, "%d ", e.StatusCode)
	} else if e.local == nil {
		b.WriteString("connection error: ")
	}
	if e.Code != "" {
		b.WriteString(e.Code + ": ")
	}
	b.WriteString(e.Message)
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request %s)", e.RequestID)
	}
	return b.String()
}

// Unwrap returns the cause of a connection error.
func (e *Error) Unwrap() error { return e.cause }

// Is reports whether target is the kind of e, such as ErrNotFound.
func (e *Error) Is(target error) bool {
	k := e.kind()
	return k != nil && k == target
}

func (e *Error) kind() error {
	switch s := e.StatusCode; {
	case e.local != nil:
		return e.local
	case s == 0:
		return ErrConnection
	case s == 401:
		return ErrAuthentication
	case s == 403:
		return ErrPermission
	case s == 404:
		return ErrNotFound
	case s == 409:
		return ErrConflict
	case s == 400 || s == 413 || s == 422:
		return ErrValidation
	case s == 429:
		return ErrRateLimit
	case s >= 500:
		return ErrServer
	}
	return nil
}

// RetryAfter is how long the API asked to wait (the Retry-After header), when it said.
func (e *Error) RetryAfter() (time.Duration, bool) {
	s := retryAfterSeconds(e.Header)
	if s == nil {
		return 0, false
	}
	if *s >= float64(math.MaxInt64)/float64(time.Second) { // an absurd Retry-After: the longest Duration
		return time.Duration(math.MaxInt64), true
	}
	return time.Duration(*s * float64(time.Second)), true
}

// IsAuthentication reports whether err is a 401: the API key is missing, wrong or revoked.
func IsAuthentication(err error) bool { return errors.Is(err, ErrAuthentication) }

// IsPermission reports whether err is a 403: the key may not do this.
func IsPermission(err error) bool { return errors.Is(err, ErrPermission) }

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsConflict reports whether err is a 409.
func IsConflict(err error) bool { return errors.Is(err, ErrConflict) }

// IsValidation reports whether err is a 400, 413 or 422: the request is invalid.
func IsValidation(err error) bool { return errors.Is(err, ErrValidation) }

// IsRateLimit reports whether err is a 429: a rate limit or a spent quota.
func IsRateLimit(err error) bool { return errors.Is(err, ErrRateLimit) }

// IsServer reports whether err is a 5xx.
func IsServer(err error) bool { return errors.Is(err, ErrServer) }

// IsConnection reports whether err is a request that got no answer: a network failure or a timeout.
func IsConnection(err error) bool { return errors.Is(err, ErrConnection) }

// errorFor reads an error answer: {"error": {"code", "message", "type"}}, or the
// flat {"name", "message"}.
func errorFor(status int, body []byte, header http.Header) *Error {
	var b struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	decodeLenient(body, &b)
	e := &Error{StatusCode: status, Code: b.Error.Code, Message: b.Error.Message, Type: b.Error.Type,
		RequestID: header.Get("X-Request-Id"), Header: header, Body: body}
	if e.Code == "" {
		e.Code = b.Name
	}
	if e.Message == "" {
		e.Message = b.Message
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("HTTP %d", status)
	}
	return e
}
