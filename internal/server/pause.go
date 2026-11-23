package server

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxPause caps a pause taken from a reset time upstream gives, so a wrong
// reset time cannot hold the proxy back for long. A request after the cap
// asks upstream again.
const maxPause = time.Hour

// pause remembers until when the plan's usage limit is known to hold.
type pause struct {
	mu    sync.Mutex
	until time.Time
}

func (p *proxy) now() time.Time {
	if p.opts.Now != nil {
		return p.opts.Now()
	}
	return time.Now()
}

// left is how long the pause still has to run at now.
func (ps *pause) left(now time.Time) time.Duration {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.until.Sub(now)
}

// extend moves the end of the pause to now+d unless it already runs longer.
func (ps *pause) extend(now time.Time, d time.Duration) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if end := now.Add(d); end.After(ps.until) {
		ps.until = end
	}
}

// paused answers a model request with the usage-limit 429 at once while the
// plan's usage limit is known to hold, and arms the pause when upstream
// reports the limit. A request after the pause goes to upstream again, and
// the first of them that finds the limit still spent starts the next pause.
func (p *proxy) paused(next http.HandlerFunc) http.HandlerFunc {
	if p.opts.UsageLimitPause <= 0 {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if left := p.pause.left(p.now()); left > 0 {
			wait := int(math.Ceil(left.Seconds()))
			note := noteOf(w)
			note.errorCode = usageLimitCode
			note.usageLimit = true
			note.paused = true
			note.retryAfter = wait
			w.Header().Set("Retry-After", strconv.Itoa(wait))
			w.Header().Set("x-should-retry", "false")
			writeError(w, http.StatusTooManyRequests, "insufficient_quota", usageLimitCode,
				"the ChatGPT plan usage limit is exhausted")
			return
		}
		next(w, r)
		if note := noteOf(w); note.usageLimit {
			d := p.opts.UsageLimitPause
			if note.retryAfter > 0 {
				d = min(time.Duration(note.retryAfter)*time.Second, maxPause)
			}
			p.pause.extend(p.now(), d)
		}
	}
}
