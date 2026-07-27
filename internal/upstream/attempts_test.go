package upstream

import (
	"errors"
	"strings"
	"testing"
)

// "all upstreams failed … last error: dial registry-1.docker.io: i/o timeout"
// named only the last hop. In CN the last hop is the official upstream nothing
// can reach, so every chain failure read as "the origin is down" and the CN
// mirror's real reason was invisible — which is how an afternoon goes into
// debugging the wrong upstream.
func TestFetchErrorNamesEveryUpstreamNotJustTheLast(t *testing.T) {
	attempts := []attemptNote{
		{Upstream: "daocloud", Err: errors.New("403 Forbidden")},
		{Upstream: "dockerhub", Err: errors.New("dial tcp 31.13.95.34:443: i/o timeout")},
	}
	got := resolveFetchError(nil, attempts[len(attempts)-1].Err, attempts).Error()

	for _, want := range []string{"daocloud", "403 Forbidden", "dockerhub", "i/o timeout"} {
		if !strings.Contains(got, want) {
			t.Errorf("error %q does not mention %q", got, want)
		}
	}
}

// The error's semantics must not change: a definitive status still wins over a
// later transport failure, because that is what keeps a 404 from becoming a 502.
func TestSummaryDoesNotChangeWhichErrorIsWrapped(t *testing.T) {
	se := &StatusError{StatusCode: 404}
	err := resolveFetchError(se, errors.New("timeout"), []attemptNote{
		{Upstream: "goproxy-cn", Err: se},
		{Upstream: "golang-proxy", Err: errors.New("timeout")},
	})
	var got *StatusError
	if !errors.As(err, &got) {
		t.Fatalf("StatusError no longer recoverable from %v", err)
	}
	if got.StatusCode != 404 {
		t.Errorf("wrapped status = %d, want 404", got.StatusCode)
	}
	if !strings.Contains(err.Error(), "goproxy-cn") {
		t.Errorf("summary missing the mirror: %v", err)
	}
}

func TestSummaryIsOmittedWhenThereIsNothingToSay(t *testing.T) {
	if got := summariseAttempts(nil); got != "" {
		t.Errorf("summariseAttempts(nil) = %q, want empty", got)
	}
	err := resolveFetchError(nil, errors.New("boom"), nil)
	if strings.Contains(err.Error(), "tried") {
		t.Errorf("empty attempt list still rendered a summary: %v", err)
	}
}

// A nil error in the list must not panic the formatter.
func TestSummaryToleratesANilError(t *testing.T) {
	got := summariseAttempts([]attemptNote{{Upstream: "mirror", Err: nil}})
	if !strings.Contains(got, "mirror") {
		t.Errorf("summary = %q", got)
	}
}

// The reported production failure: a CN cluster's Hub chain was DaoCloud → Docker
// Hub. DaoCloud tripped its circuit breaker, so every request skipped it silently
// and fell to the official origin, which CN cannot reach. The error said only
// "dial registry-1.docker.io: i/o timeout", which reads as "Docker Hub is down"
// and hides both the real cause (a broken mirror) and the structural one (nothing
// else to fall through to).
func TestSkippedCircuitBrokenUpstreamAppearsInTheError(t *testing.T) {
	err := resolveFetchError(nil, errors.New("dial tcp 31.13.95.34:443: i/o timeout"),
		[]attemptNote{
			{Upstream: "daocloud", Err: errCircuitOpen},
			{Upstream: "dockerhub", Err: errors.New("dial tcp 31.13.95.34:443: i/o timeout")},
		})
	msg := err.Error()
	for _, want := range []string{"daocloud", "circuit breaker open", "dockerhub", "i/o timeout"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}
