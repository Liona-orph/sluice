package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Liona-orph/sluice/internal/router"
	"github.com/Liona-orph/sluice/pkg/llm"
)

// OutcomeClientDisconnected is the audit and metric outcome for a stream the
// client abandoned. It is not a Kind, because it is not a failure the client
// will ever see -- by definition the client is gone -- and counting it as one
// would make an error-rate dashboard react to users closing tabs.
const OutcomeClientDisconnected = "client_disconnected"

// Kind classifies a gateway failure for the client.
//
// The values are OpenAI's error `type` strings, because a client library that
// already speaks to OpenAI has error handling written against them, and the
// point of the compatible surface is that such a client works unchanged. Where
// Sluice has a failure OpenAI does not, it reuses the closest existing type
// rather than inventing one: an unrecognised type is the case client libraries
// handle worst.
type Kind string

const (
	// KindInvalidRequest is a malformed or unsupported request.
	KindInvalidRequest Kind = "invalid_request_error"
	// KindAuthentication is a missing or unrecognised key.
	KindAuthentication Kind = "authentication_error"
	// KindPermission is a valid key asking for something it may not have.
	KindPermission Kind = "permission_error"
	// KindRateLimit is a token bucket refusing.
	KindRateLimit Kind = "rate_limit_error"
	// KindBudget is a spend limit refusing. OpenAI calls this
	// insufficient_quota and returns 429; Sluice returns 402, because 429 means
	// "try again shortly" and a budget rejection is not fixed by waiting a few
	// seconds. Clients that only special-case 429 still see a non-retryable 4xx.
	KindBudget Kind = "insufficient_quota"
	// KindUpstream is a provider failure that survived retry and failover.
	KindUpstream Kind = "api_error"
	// KindInternal is a bug in Sluice.
	KindInternal Kind = "internal_error"
)

// Error is a failure with an HTTP status attached.
type Error struct {
	Status  int
	Kind    Kind
	Message string
	// Code is the machine-readable sub-code, mirroring llm.ErrorCode where the
	// failure came from a provider.
	Code string
	// RetryAfter populates the header of the same name when non-zero.
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return e.Err }

// FromProvider maps a provider or router error onto a client-facing one.
//
// The mapping is deliberately lossy in one direction: a caller learns the class
// of failure and, for the classes where it helps, how long to wait. It does not
// learn which upstream failed or what it said. Provider identity and provider
// error text belong in the audit log and the operator's dashboard, not in a
// response to an application that has no business knowing which vendor is
// behind the alias it called.
func FromProvider(err error) *Error {
	if err == nil {
		return nil
	}
	var ge *Error
	if errors.As(err, &ge) {
		return ge
	}
	if errors.Is(err, router.ErrNoRoute) {
		return &Error{
			Status: http.StatusNotFound, Kind: KindInvalidRequest,
			Message: "the model does not exist or you do not have access to it", Err: err,
		}
	}

	code := llm.CodeOf(err)
	out := &Error{Code: string(code), Err: err}
	if after, ok := llm.RetryAfter(err); ok {
		out.RetryAfter = after
	}
	switch code {
	case llm.CodeRateLimited:
		out.Status, out.Kind = http.StatusTooManyRequests, KindRateLimit
		out.Message = "upstream provider is rate limiting this gateway"
	case llm.CodeContextLengthExceeded:
		out.Status, out.Kind = http.StatusBadRequest, KindInvalidRequest
		out.Message = "the request exceeds the model's context length"
	case llm.CodeContentFiltered:
		out.Status, out.Kind = http.StatusBadRequest, KindInvalidRequest
		out.Message = "the request was refused by the provider's content filter"
	case llm.CodeAuthentication:
		// The gateway's credentials are wrong, not the caller's. A 401 here
		// would send the client to rotate a key that is working fine, so it is
		// reported as what it is: this server is misconfigured.
		out.Status, out.Kind = http.StatusBadGateway, KindUpstream
		out.Message = "the gateway could not authenticate to any upstream provider"
	case llm.CodeInvalidRequest:
		out.Status, out.Kind = http.StatusBadRequest, KindInvalidRequest
		out.Message = "the request was rejected as invalid by the provider"
	case llm.CodeTimeout:
		out.Status, out.Kind = http.StatusGatewayTimeout, KindUpstream
		out.Message = "the upstream provider did not answer in time"
	case llm.CodeProviderUnavailable:
		out.Status, out.Kind = http.StatusBadGateway, KindUpstream
		out.Message = "no upstream provider is available for this model"
	default:
		out.Status, out.Kind = http.StatusBadGateway, KindUpstream
		out.Message = "the request could not be completed"
	}
	return out
}

// Invalid builds a 400.
func Invalid(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Kind: KindInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

// Forbidden builds a 403.
func Forbidden(format string, args ...any) *Error {
	return &Error{Status: http.StatusForbidden, Kind: KindPermission, Message: fmt.Sprintf(format, args...)}
}
