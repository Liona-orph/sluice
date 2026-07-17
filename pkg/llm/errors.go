package llm

import (
	"errors"
	"fmt"
	"time"
)

// ErrorCode classifies a provider failure.
//
// The taxonomy exists to answer two questions the gateway has to answer without
// reading an error string: may we try again against the same provider
// (Retryable), and is a different provider likely to do better (Failover).
// Those are independent -- an expired API key is not retryable but is exactly
// what a second provider is for -- so both are encoded.
type ErrorCode string

const (
	// CodeRateLimited means the provider is throttling us. Retryable after the
	// interval it names; another provider has its own quota.
	CodeRateLimited ErrorCode = "rate_limited"
	// CodeContextLengthExceeded means the prompt plus requested completion does not
	// fit the model's window. Retrying sends the same oversized prompt, and
	// failing over sends it to a model the router already ranked lower; the
	// caller has to shorten it or pick a larger model explicitly.
	CodeContextLengthExceeded ErrorCode = "context_length_exceeded"
	// CodeContentFiltered means the provider refused on safety grounds. Not
	// retryable. Deliberately not failover-worthy: shopping the same prompt
	// around until a provider accepts it is a policy decision an operator must
	// make explicitly, not a default the gateway takes on their behalf.
	CodeContentFiltered ErrorCode = "content_filtered"
	// CodeAuthentication means credentials were rejected or missing. Retrying with the
	// same key fails identically, but a second provider with working
	// credentials is precisely the intended remedy.
	CodeAuthentication ErrorCode = "authentication"
	// CodeProviderUnavailable covers 5xx, connection refused and overload. Retryable
	// and the canonical failover case.
	CodeProviderUnavailable ErrorCode = "provider_unavailable"
	// CodeInvalidRequest means the request is malformed or asks for something the
	// model does not support. Every provider will reject it the same way.
	CodeInvalidRequest ErrorCode = "invalid_request"
	// CodeTimeout means the deadline elapsed. Retryable only if the caller's own
	// deadline still allows it, which is the caller's judgement, not ours.
	CodeTimeout ErrorCode = "timeout"
	// CodeUnknown covers anything unclassified. Treated as neither retryable nor
	// failover-worthy, because guessing wrong on an unknown error means either
	// duplicating a side effect or amplifying load during an incident.
	CodeUnknown ErrorCode = "unknown"
)

// Retryable reports whether the same provider may be asked again.
func (c ErrorCode) Retryable() bool {
	switch c {
	case CodeRateLimited, CodeProviderUnavailable, CodeTimeout:
		return true
	default:
		// Everything else describes the request rather than a transient
		// condition, so the same request will fail the same way. The default is
		// written out so that a new code has to be classified deliberately.
		return false
	}
}

// Failover reports whether a different provider is worth trying.
func (c ErrorCode) Failover() bool {
	switch c {
	case CodeRateLimited, CodeProviderUnavailable, CodeTimeout, CodeAuthentication:
		return true
	default:
		return false
	}
}

func (c ErrorCode) String() string { return string(c) }

// Sentinel errors for use with errors.Is. They carry no context; the concrete
// *Error does. Matching on a sentinel is the common case ("is this a rate
// limit?"), so it should not require a type assertion.
var (
	ErrRateLimited           error = codeError(CodeRateLimited)
	ErrContextLengthExceeded error = codeError(CodeContextLengthExceeded)
	ErrContentFiltered       error = codeError(CodeContentFiltered)
	ErrAuthentication        error = codeError(CodeAuthentication)
	ErrProviderUnavailable   error = codeError(CodeProviderUnavailable)
	ErrInvalidRequest        error = codeError(CodeInvalidRequest)
	ErrTimeout               error = codeError(CodeTimeout)
	ErrUnknown               error = codeError(CodeUnknown)
)

type codeError ErrorCode

func (c codeError) Error() string { return "llm: " + string(c) }

// Error is the concrete provider error. Adapters construct it; the router,
// retry loop and cost accountant read it.
type Error struct {
	Code ErrorCode
	// Provider and Model identify where the failure happened. They are part of
	// the error rather than added by the caller because by the time an error
	// reaches a retry loop the caller has already forgotten which of three
	// failover candidates produced it.
	Provider string
	Model    string
	// Message is the provider's own description, kept verbatim. It is the only
	// thing that makes an unknown-code error debuggable.
	Message string
	// RetryAfter is the delay the provider asked for, zero if it said nothing.
	// A rate limit without a stated interval is common; the retry policy
	// supplies a backoff in that case.
	RetryAfter time.Duration
	// StatusCode is the HTTP status where there was one, for logs.
	StatusCode int
	// Err is the underlying cause, e.g. a net.Error or context.DeadlineExceeded.
	Err error
}

func (e *Error) Error() string {
	where := e.Provider
	if e.Model != "" {
		where += "/" + e.Model
	}
	if where == "" {
		where = "llm"
	}
	if e.Message == "" {
		return fmt.Sprintf("%s: %s", where, e.Code)
	}
	return fmt.Sprintf("%s: %s: %s", where, e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrRateLimited) work on a fully populated *Error.
func (e *Error) Is(target error) bool {
	c, ok := target.(codeError)
	return ok && ErrorCode(c) == e.Code
}

// Retryable forwards to the code, so callers holding an *Error do not have to
// reach through to it.
func (e *Error) Retryable() bool { return e.Code.Retryable() }

// Failover forwards to the code, for the same reason.
func (e *Error) Failover() bool { return e.Code.Failover() }

// Errorf builds an *Error with a formatted message.
func Errorf(code ErrorCode, provider, model, format string, args ...any) *Error {
	return &Error{Code: code, Provider: provider, Model: model, Message: fmt.Sprintf(format, args...)}
}

// CodeOf extracts the classification from err, walking the wrap chain.
//
// Unclassified errors report CodeUnknown rather than an "is this even ours"
// boolean: every caller would have to handle the false case as unknown anyway.
func CodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	var c codeError
	if errors.As(err, &c) {
		return ErrorCode(c)
	}
	return CodeUnknown
}

// IsRetryable reports whether err permits another attempt against the same
// provider.
func IsRetryable(err error) bool { return CodeOf(err).Retryable() }

// ShouldFailover reports whether err justifies trying a different provider.
func ShouldFailover(err error) bool { return CodeOf(err).Failover() }

// RetryAfter returns the delay the provider requested, if it requested one.
func RetryAfter(err error) (time.Duration, bool) {
	var e *Error
	if errors.As(err, &e) && e.RetryAfter > 0 {
		return e.RetryAfter, true
	}
	return 0, false
}
