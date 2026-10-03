package github

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

// Retries is how GitHub requests are retried: a request that fails for a reason that may pass is sent
// again, up to Max more times, after a wait that doubles from WaitMin up to WaitMax, or what GitHub's
// Retry-After asks for.
type Retries struct {
	Max              int
	WaitMin, WaitMax time.Duration
}

// DefaultRetries rides out a GitHub blip of about half a minute: 6 attempts, 31 s of waits between
// them, at most 3.5 minutes with every attempt timing out. A longer outage fails the call, and the
// event's failure is reported, with retries of its own.
var DefaultRetries = Retries{Max: 5, WaitMin: time.Second, WaitMax: 30 * time.Second}

// maxRetryAfter caps the wait a Retry-After asks for (a primary rate limit's can be up to an hour), so
// one request can't hold a webhook job or the leader's loops for long.
const maxRetryAfter = time.Minute

// retrying wraps rt so that GitHub requests are retried per r: connection errors and timeouts, 5xx
// responses (but 501), 429 and GitHub's secondary rate limit (403 with Retry-After). Every retry is
// logged with its attempt and cause; a request that still fails returns its last response or error,
// for the caller to handle and log.
func retrying(rt http.RoundTripper, r Retries, logger *slog.Logger) http.RoundTripper {
	c := retryablehttp.NewClient()
	c.HTTPClient = &http.Client{Transport: rt, Timeout: requestTimeout}
	c.Logger = nil
	c.RetryMax, c.RetryWaitMin, c.RetryWaitMax = r.Max, r.WaitMin, r.WaitMax
	c.Backoff = backoff
	c.ErrorHandler = retryablehttp.PassthroughErrorHandler
	c.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		retry, perr := retryablehttp.DefaultRetryPolicy(ctx, resp, err)
		if !retry && perr == nil && secondaryRateLimited(resp) {
			retry = true
		}
		a, _ := ctx.Value(attemptsKey{}).(*attempts)
		if a == nil {
			return retry, perr
		}
		a.n++
		if retry && a.n <= r.Max {
			logger.WarnContext(ctx, "GitHub request failed; retrying", "method", a.method, "path", a.path,
				"attempt", a.n, "attempts", r.Max+1, "cause", cause(resp, err), "wait", backoff(r.WaitMin, r.WaitMax, a.n-1, resp).String())
		}
		return retry, perr
	}
	return &tracking{next: &retryablehttp.RoundTripper{Client: c}}
}

// attempts counts the attempts of one request, for its retries' logs.
type attempts struct {
	method, path string
	n            int
}

type attemptsKey struct{}

// tracking gives each request its own attempts counter.
type tracking struct{ next http.RoundTripper }

func (t *tracking) RoundTrip(req *http.Request) (*http.Response, error) {
	a := &attempts{method: req.Method, path: req.URL.Path}
	return t.next.RoundTrip(req.WithContext(context.WithValue(req.Context(), attemptsKey{}, a)))
}

// backoff is retryablehttp's exponential backoff, which honors Retry-After on 429 and 503, and also
// honors it on a secondary rate limit; a Retry-After is followed up to maxRetryAfter.
func backoff(minWait, maxWait time.Duration, attempt int, resp *http.Response) time.Duration {
	if secondaryRateLimited(resp) {
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
			return min(time.Duration(s)*time.Second, maxRetryAfter)
		}
	}
	return min(retryablehttp.DefaultBackoff(minWait, maxWait, attempt, resp), maxRetryAfter)
}

// secondaryRateLimited reports whether resp is GitHub's secondary rate limit: a 403 that says when to
// try again.
func secondaryRateLimited(resp *http.Response) bool {
	return resp != nil && resp.StatusCode == http.StatusForbidden && resp.Header.Get("Retry-After") != ""
}

func cause(resp *http.Response, err error) string {
	if err != nil {
		return err.Error()
	}
	return resp.Status
}
