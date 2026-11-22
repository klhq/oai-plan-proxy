package tokens_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/oauth"
	"github.com/klhq/oai-plan-proxy/internal/tokens"
)

// logBuffer collects JSON log lines from concurrent writers.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records returns every parsed record.
func (b *logBuffer) records() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var all []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil {
			all = append(all, rec)
		}
	}
	return all
}

// loggedManager rebuilds f.mgr with a JSON logger and an optional timer.
func (f *fixture) loggedManager(after func(time.Duration) <-chan time.Time) *logBuffer {
	logs := &logBuffer{}
	f.mgr = tokens.New(tokens.Options{
		Store:  f.store,
		Client: f.auth.Client(),
		Endpoints: oauth.Endpoints{
			TokenURL: f.auth.URL + "/token",
			Resource: "https://api.example.test/v1",
		},
		Now:    f.clock.Now,
		After:  after,
		Logger: slog.New(slog.NewJSONHandler(logs, nil)),
	})
	return logs
}

func TestARefreshOnTheRequestPathLogsItsReasonAndExpiry(t *testing.T) {
	f := newFixture(t, start.Add(-time.Minute))
	logs := f.loggedManager(nil)

	if _, err := f.mgr.AccessToken(context.Background()); err != nil {
		t.Fatal(err)
	}

	recs := logs.records()
	want := start.Add(time.Hour).Format(time.RFC3339)
	if len(recs) != 1 || recs[0]["msg"] != "token refreshed" || recs[0]["level"] != "INFO" ||
		recs[0]["reason"] != "expired" || recs[0]["expires_at"] != want {
		t.Fatalf("records = %v, want one token refreshed with reason expired and expires_at %s", recs, want)
	}
}

func TestAScheduledRefreshLogsTheScheduledReason(t *testing.T) {
	f := newFixture(t, start.Add(time.Hour))
	tm := newTimer()
	logs := f.loggedManager(tm.After)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.mgr.Run(ctx)

	tm.nextWait(t)
	f.clock.Set(start.Add(time.Hour - tokens.RefreshMargin))
	tm.fire <- f.clock.Now()
	tm.nextWait(t)

	recs := logs.records()
	if len(recs) != 1 || recs[0]["msg"] != "token refreshed" || recs[0]["reason"] != "scheduled" {
		t.Fatalf("records = %v, want one token refreshed with reason scheduled", recs)
	}
}

func TestARefreshAfterAnUpstream401LogsThatReason(t *testing.T) {
	f := newFixture(t, start.Add(time.Hour))
	logs := f.loggedManager(nil)

	if _, err := f.mgr.RefreshRejected(context.Background(), "access-1"); err != nil {
		t.Fatal(err)
	}

	recs := logs.records()
	if len(recs) != 1 || recs[0]["reason"] != "upstream_401" {
		t.Fatalf("records = %v, want one token refreshed with reason upstream_401", recs)
	}
}

func TestATransientRefreshFailureIsLoggedAsAWarning(t *testing.T) {
	f := newFixture(t, start.Add(-time.Minute))
	f.auth.respond = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }
	logs := f.loggedManager(nil)

	_, _ = f.mgr.AccessToken(context.Background())

	recs := logs.records()
	if len(recs) != 1 {
		t.Fatalf("records = %v, want one", recs)
	}
	errText, _ := recs[0]["error"].(string)
	if recs[0]["msg"] != "token refresh failed" || recs[0]["level"] != "WARN" ||
		recs[0]["reason"] != "expired" || !strings.Contains(errText, "503") {
		t.Fatalf("records = %v, want one token refresh failed warning with the error", recs)
	}
}

func TestARejectedRefreshTokenLogsSignInDeadOnce(t *testing.T) {
	f := newFixture(t, start.Add(-time.Minute))
	f.auth.respond = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}
	logs := f.loggedManager(nil)

	_, _ = f.mgr.AccessToken(context.Background())
	_, _ = f.mgr.AccessToken(context.Background())
	_, _ = f.mgr.RefreshRejected(context.Background(), "access-1")

	recs := logs.records()
	if len(recs) != 1 {
		t.Fatalf("records = %v, want exactly one", recs)
	}
	hint, _ := recs[0]["hint"].(string)
	if recs[0]["msg"] != "sign-in dead" || recs[0]["level"] != "ERROR" ||
		!strings.Contains(hint, "oai-plan-proxy login") || !strings.Contains(hint, "import") {
		t.Fatalf("record = %v, want sign-in dead at error with a login and import hint", recs[0])
	}
}
