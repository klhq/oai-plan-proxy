package server_test

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/server"
)

// clock is a settable time source.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

// pausedProxy builds the handler with a JSON logger, a usage-limit pause of
// pause, and c as its clock.
func pausedProxy(u *fakeUpstream, pause time.Duration, c *clock) (http.Handler, *logBuffer) {
	logs := &logBuffer{}
	return server.New(server.Options{
		Login:           fakeLogin{state: auth.StateValid},
		Tokens:          &fakeTokens{current: "access-1"},
		Upstream:        u.URL + "/v1",
		Client:          u.Client(),
		UsageLimitPause: pause,
		Now:             c.Now,
		Logger:          slog.New(slog.NewJSONHandler(logs, nil)),
	}), logs
}

const usageLimitBody = `{"error":{"message":"Plan usage limit reached.","type":"insufficient_quota","code":"` + usageLimit + `"}}`

func TestAfterTheUsageLimitRequestsGetTheSame429WithoutReachingUpstream(t *testing.T) {
	u := limitUpstream(t, usageLimitBody, func(http.Header) {})
	h, _ := pausedProxy(u, time.Minute, &clock{now: time.Unix(1_800_000_000, 0)})
	body := `{"model":"gpt-5","input":"hi"}`
	post(t, h, "/v1/responses", body)

	for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
		rec := post(t, h, path, `{"model":"gpt-5","input":"hi","messages":[{"role":"user","content":"hi"}]}`)
		assertUsageLimit(t, rec)
	}

	if calls := u.received(); len(calls) != 1 {
		t.Fatalf("upstream received %d requests, want only the first", len(calls))
	}
}

func TestThePauseEndsAndTheNextRequestAsksUpstreamAgain(t *testing.T) {
	u := limitUpstream(t, usageLimitBody, func(http.Header) {})
	c := &clock{now: time.Now()}
	h, _ := pausedProxy(u, 30*time.Second, c)
	body := `{"model":"gpt-5","input":"hi"}`
	post(t, h, "/v1/responses", body)

	c.now = c.now.Add(10 * time.Second)
	during := post(t, h, "/v1/responses", body)
	c.now = c.now.Add(21 * time.Second)
	post(t, h, "/v1/responses", body)

	assertUsageLimit(t, during)
	if got := during.Header().Get("Retry-After"); got != "20" {
		t.Errorf("Retry-After during the pause = %q, want 20", got)
	}
	if calls := u.received(); len(calls) != 2 {
		t.Fatalf("upstream received %d requests, want 2: the first, and one after the pause", len(calls))
	}
}

func TestAResetTimeFromUpstreamSetsHowLongThePauseLasts(t *testing.T) {
	c := &clock{now: time.Now()}
	body := `{"error":{"type":"usage_limit_reached","message":"limit","resets_at":` + strconv.FormatInt(c.now.Add(10*time.Minute).Unix(), 10) + `}}`
	u := limitUpstream(t, body, func(http.Header) {})
	h, _ := pausedProxy(u, 30*time.Second, c)
	req := `{"model":"gpt-5","input":"hi"}`
	post(t, h, "/v1/responses", req)

	c.now = c.now.Add(5 * time.Minute)
	rec := post(t, h, "/v1/responses", req)

	assertUsageLimit(t, rec)
	if got, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || got < 298 || got > 301 {
		t.Errorf("Retry-After = %q, want about 300: the reset is 5 minutes away", rec.Header().Get("Retry-After"))
	}
	if calls := u.received(); len(calls) != 1 {
		t.Fatalf("upstream received %d requests, want 1: a 30s pause would have ended", len(calls))
	}
}

func TestAFarResetTimeOnlyPausesForAnHour(t *testing.T) {
	c := &clock{now: time.Now()}
	body := `{"error":{"type":"usage_limit_reached","message":"limit","resets_at":` + strconv.FormatInt(c.now.Add(5*time.Hour).Unix(), 10) + `}}`
	u := limitUpstream(t, body, func(http.Header) {})
	h, _ := pausedProxy(u, 30*time.Second, c)
	req := `{"model":"gpt-5","input":"hi"}`
	post(t, h, "/v1/responses", req)

	c.now = c.now.Add(59 * time.Minute)
	post(t, h, "/v1/responses", req)
	held := len(u.received())
	c.now = c.now.Add(2 * time.Minute)
	post(t, h, "/v1/responses", req)

	if held != 1 || len(u.received()) != 2 {
		t.Fatalf("upstream received %d requests at 59 min and %d at 61 min, want 1 and 2", held, len(u.received()))
	}
}

func TestWithoutAPauseEveryRequestStillAsksUpstream(t *testing.T) {
	c := &clock{now: time.Now()}
	body := `{"error":{"type":"usage_limit_reached","message":"limit","resets_at":` + strconv.FormatInt(c.now.Add(10*time.Minute).Unix(), 10) + `}}`
	u := limitUpstream(t, body, func(http.Header) {})
	h, _ := pausedProxy(u, 0, c)
	req := `{"model":"gpt-5","input":"hi"}`

	post(t, h, "/v1/responses", req)
	post(t, h, "/v1/responses", req)

	if calls := u.received(); len(calls) != 2 {
		t.Fatalf("upstream received %d requests, want 2: a zero pause turns the feature off", len(calls))
	}
}

func TestThePauseOnlyHoldsBackModelRequests(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, usageLimitBody)
	})
	h, _ := pausedProxy(u, time.Minute, &clock{now: time.Now()})
	post(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

	models := get(t, h, "/v1/models")
	health := get(t, h, "/healthz")

	if models.Code != http.StatusOK || health.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, GET /healthz = %d, want 200 for both during the pause", models.Code, health.Code)
	}
}

func TestARequestHeldByThePauseIsLoggedAsSuch(t *testing.T) {
	u := limitUpstream(t, usageLimitBody, func(http.Header) {})
	h, logs := pausedProxy(u, time.Minute, &clock{now: time.Now()})
	req := `{"model":"gpt-5","input":"hi"}`
	post(t, h, "/v1/responses", req)
	post(t, h, "/v1/responses", req)

	recs := logs.records(t, "request")

	if len(recs) != 2 {
		t.Fatalf("request records = %d, want 2", len(recs))
	}
	if recs[0]["paused"] != nil {
		t.Errorf("the request that reached upstream has paused = %v, want none", recs[0]["paused"])
	}
	held := recs[1]
	if held["paused"] != true || held["level"] != "WARN" || held["error_code"] != usageLimit || held["retry_after_s"] != float64(60) {
		t.Errorf("held request record = %v, want paused true, level WARN, error_code %s, retry_after_s 60", held, usageLimit)
	}
}
