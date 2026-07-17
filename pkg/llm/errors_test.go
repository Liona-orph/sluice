package llm

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The retry/failover matrix is the whole point of the taxonomy, so it is
// asserted exhaustively rather than sampled. Changing a cell here changes how
// the gateway behaves during an incident.
func TestErrorCodePolicy(t *testing.T) {
	for _, tc := range []struct {
		code                ErrorCode
		retryable, failover bool
	}{
		{CodeRateLimited, true, true},
		{CodeContextLengthExceeded, false, false},
		{CodeContentFiltered, false, false},
		{CodeAuthentication, false, true},
		{CodeProviderUnavailable, true, true},
		{CodeInvalidRequest, false, false},
		{CodeTimeout, true, true},
		{CodeUnknown, false, false},
	} {
		t.Run(string(tc.code), func(t *testing.T) {
			assert.Equal(t, tc.retryable, tc.code.Retryable())
			assert.Equal(t, tc.failover, tc.code.Failover())
		})
	}
}

func TestErrorSentinelMatching(t *testing.T) {
	err := &Error{Code: CodeRateLimited, Provider: "openai", Model: "gpt-4o", RetryAfter: 2 * time.Second}

	require.ErrorIs(t, err, ErrRateLimited)
	require.NotErrorIs(t, err, ErrTimeout)
	require.ErrorIs(t, fmt.Errorf("routing: %w", err), ErrRateLimited,
		"wrapping must not hide the classification")

	d, ok := RetryAfter(err)
	require.True(t, ok)
	assert.Equal(t, 2*time.Second, d)
}

func TestErrorUnwrapAndClassify(t *testing.T) {
	cause := context.DeadlineExceeded
	err := &Error{Code: CodeTimeout, Provider: "local", Message: "deadline", Err: cause}

	require.ErrorIs(t, err, context.DeadlineExceeded, "the cause must stay reachable")
	assert.Equal(t, CodeTimeout, CodeOf(err))
	assert.True(t, IsRetryable(err))
	assert.True(t, ShouldFailover(err))
}

func TestCodeOfDegradesUnknownErrors(t *testing.T) {
	assert.Equal(t, ErrorCode(""), CodeOf(nil))
	assert.Equal(t, CodeUnknown, CodeOf(errors.New("something went wrong")))
	// An unclassified error must not be retried: guessing costs either a
	// duplicated side effect or amplified load.
	assert.False(t, IsRetryable(errors.New("boom")))
	assert.False(t, ShouldFailover(errors.New("boom")))
}

func TestSentinelIsUsableAlone(t *testing.T) {
	// A provider that has nothing to add may return the sentinel directly.
	assert.Equal(t, CodeContentFiltered, CodeOf(ErrContentFiltered))
	assert.ErrorIs(t, fmt.Errorf("wrapped: %w", ErrContentFiltered), ErrContentFiltered)
}

func TestRetryAfterAbsent(t *testing.T) {
	_, ok := RetryAfter(&Error{Code: CodeRateLimited})
	assert.False(t, ok, "a rate limit with no stated interval must not report one")
	_, ok = RetryAfter(errors.New("plain"))
	assert.False(t, ok)
}

func TestErrorMessage(t *testing.T) {
	for _, tc := range []struct {
		err  *Error
		want string
	}{
		{&Error{Code: CodeTimeout}, "llm: timeout"},
		{&Error{Code: CodeTimeout, Provider: "openai"}, "openai: timeout"},
		{Errorf(CodeInvalidRequest, "openai", "gpt-4o", "bad %s", "field"),
			"openai/gpt-4o: invalid_request: bad field"},
	} {
		assert.Equal(t, tc.want, tc.err.Error())
	}
}
