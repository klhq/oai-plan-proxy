package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"
)

// usageWindow is one plan usage window as upstream reports it in the
// x-codex-<name>-* headers that Codex reads.
type usageWindow struct {
	name          string
	usedPercent   float64
	windowMinutes *int64
	resetsAt      *time.Time
}

// usageWindows reads the primary and secondary usage windows from upstream
// headers. A window without a parsable used percent is left out.
func usageWindows(h http.Header) []usageWindow {
	var ws []usageWindow
	for _, name := range []string{"primary", "secondary"} {
		prefix := "x-codex-" + name + "-"
		used, err := strconv.ParseFloat(h.Get(prefix+"used-percent"), 64)
		if err != nil || math.IsNaN(used) || math.IsInf(used, 0) {
			continue
		}
		w := usageWindow{name: name, usedPercent: used}
		if m, err := strconv.ParseInt(h.Get(prefix+"window-minutes"), 10, 64); err == nil {
			w.windowMinutes = &m
		}
		if s, err := strconv.ParseInt(h.Get(prefix+"reset-at"), 10, 64); err == nil {
			t := time.Unix(s, 0).UTC()
			w.resetsAt = &t
		}
		ws = append(ws, w)
	}
	return ws
}

// attrs returns the window's log attributes.
func (w usageWindow) attrs() []any {
	key := "usage_" + w.name + "_"
	a := []any{key + "percent", w.usedPercent}
	if w.windowMinutes != nil {
		a = append(a, key+"window_minutes", *w.windowMinutes)
	}
	if w.resetsAt != nil {
		a = append(a, key+"resets_at", w.resetsAt.Format(time.RFC3339))
	}
	return a
}

// retryAfter is how many whole seconds from now until the usage limit
// resets, rounded up, or 0 when the reset time is unknown or past. The
// reset time is f's own when it has one. Otherwise it is the latest reset
// among the windows that are spent.
func retryAfter(f upstreamFailure, windows []usageWindow, now time.Time) int {
	var resets time.Time
	var seconds float64
	if json.Unmarshal(f.ResetsAt, &seconds) == nil && seconds > 0 {
		resets = time.UnixMilli(int64(seconds * 1000))
	} else {
		for _, w := range windows {
			if w.usedPercent >= 100 && w.resetsAt != nil && w.resetsAt.After(resets) {
				resets = *w.resetsAt
			}
		}
	}
	wait := resets.Sub(now)
	if resets.IsZero() || wait <= 0 {
		return 0
	}
	return int(math.Ceil(wait.Seconds()))
}
