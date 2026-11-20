package canarynode

import "testing"

func TestDeepSeekRateLimitBoundaries(t *testing.T) {
	if maxDeepSeekCallsPerEvent != 2 {
		t.Fatalf("per-event DeepSeek budget changed: %d", maxDeepSeekCallsPerEvent)
	}
	if maxDeepSeekCalls24h != 10 {
		t.Fatalf("24h DeepSeek budget changed: %d", maxDeepSeekCalls24h)
	}

	for _, test := range []struct {
		name  string
		scope string
		limit int
		want  string
	}{
		{name: "event", scope: "event", limit: 2, want: "DeepSeek rate limit exceeded for event: maximum 2 calls"},
		{name: "daily", scope: "daily", limit: 10, want: "DeepSeek rate limit exceeded for 24h window: maximum 10 calls"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := &deepSeekRateLimitError{scope: test.scope, limit: test.limit}
			if err.Error() != test.want {
				t.Fatalf("unexpected rate-limit error: %q", err.Error())
			}
			if got := deepSeekRateLimitLimit(deepSeekRateLimitScope(err.Error())); got != test.limit {
				t.Fatalf("unexpected parsed limit: got %d want %d", got, test.limit)
			}
		})
	}
}
