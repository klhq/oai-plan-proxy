package server

import "net/http"

// concurrencyLimitCode is the error code of a request turned away because
// MaxConcurrent requests are already in flight.
const concurrencyLimitCode = "concurrency_limit"

// limited runs next only while fewer than MaxConcurrent model requests are
// in flight. A request over the limit gets 429 at once. It leaves out
// x-should-retry, because the wait is short and a retry can succeed.
func (p *proxy) limited(next http.HandlerFunc) http.HandlerFunc {
	if p.slots == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case p.slots <- struct{}{}:
			defer func() { <-p.slots }()
			next(w, r)
		default:
			noteOf(w).errorCode = concurrencyLimitCode
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate_limit_error", concurrencyLimitCode,
				"too many requests in flight; the proxy allows OAI_PLAN_PROXY_MAX_CONCURRENT at a time")
		}
	}
}
