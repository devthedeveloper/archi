package main

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRetries is how many times a failed request is retried after the first
// attempt before giving up.
const maxRetries = 3

// retryBase is the first backoff step; each retry doubles it (1s, 2s, 4s).
const retryBase = time.Second

// retryAfterCap bounds how long a server-sent Retry-After header can make us wait.
const retryAfterCap = 30 * time.Second

// doWithRetry issues the HTTP request produced by build, retrying 429/5xx
// responses and transient transport errors up to maxRetries times with
// exponential backoff and jitter, honoring Retry-After (whole seconds,
// capped). build runs once per attempt so the request body is fresh each
// time. Retries cover only request setup and the response headers — once a
// usable response is returned the stream is never re-requested — and sleeps
// respect ctx cancellation. A non-retryable or final failing response is
// returned as-is so the caller can report its status and body.
func doWithRetry(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			if attempt >= maxRetries || !isRetryableErr(err) {
				return nil, err
			}
			if serr := sleepCtx(ctx, retryDelay(attempt, "")); serr != nil {
				return nil, serr
			}
			continue
		}
		if !isRetryableStatus(resp.StatusCode) || attempt >= maxRetries {
			return resp, nil
		}
		retryAfter := resp.Header.Get("Retry-After")
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if serr := sleepCtx(ctx, retryDelay(attempt, retryAfter)); serr != nil {
			return nil, serr
		}
	}
}

// isRetryableStatus reports whether an HTTP status code warrants a retry:
// 429 (rate limited) and any 5xx.
func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code/100 == 5
}

// isRetryableErr reports whether a transport error is worth retrying.
// Deliberate cancellation and blown deadlines are not; everything else
// (refused connections, resets, header timeouts) is treated as transient.
func isRetryableErr(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// retryDelay picks the pause before retrying a 0-based attempt: the parsed
// Retry-After when the server sent a usable one, otherwise jittered
// exponential backoff.
func retryDelay(attempt int, retryAfter string) time.Duration {
	if d, ok := parseRetryAfter(retryAfter); ok {
		return d
	}
	return backoffDelay(attempt)
}

// backoffDelay is the jittered exponential backoff for a 0-based attempt:
// uniform in [d/2, 3d/2) where d = retryBase << attempt.
func backoffDelay(attempt int) time.Duration {
	d := retryBase << attempt
	return d/2 + time.Duration(rand.Int64N(int64(d)))
}

// parseRetryAfter reads a Retry-After header given in whole seconds, capping
// the result at retryAfterCap. HTTP-date forms and garbage are ignored.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	s, err := strconv.Atoi(v)
	if err != nil || s < 0 {
		return 0, false
	}
	d := time.Duration(s) * time.Second
	if d > retryAfterCap {
		d = retryAfterCap
	}
	return d, true
}

// sleepCtx sleeps for d, returning early with ctx's error if it is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
