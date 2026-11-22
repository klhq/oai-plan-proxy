package cli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/store"
)

// logRecords parses JSON log lines and returns the records whose msg is msg.
func logRecords(t *testing.T, out, msg string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec["msg"] == msg {
			found = append(found, rec)
		}
	}
	return found
}

func TestServeJSONLogsTheStartupSettings(t *testing.T) {
	h := newHarness(t)
	h.vars["OAI_PLAN_PROXY_LOG_FORMAT"] = "json"
	h.vars["OAI_PLAN_PROXY_API_KEY"] = "secret-proxy-key"
	h.saveCredentials(t, signedIn())

	serveAndProbe(t, h)

	recs := logRecords(t, h.stderr.String(), "listening")
	if len(recs) != 1 {
		t.Fatalf("listening records = %d, want 1; stderr:\n%s", len(recs), h.stderr.String())
	}
	rec := recs[0]
	if rec["level"] != "INFO" || rec["addr"] != h.vars["OAI_PLAN_PROXY_LISTEN"] ||
		rec["upstream"] != h.upstream || rec["strict"] != false ||
		rec["api_key_set"] != true || rec["login"] != "valid" || rec["time"] == nil {
		t.Fatalf("listening record = %v", rec)
	}
}

func TestServeRejectsAnUnknownLogFormat(t *testing.T) {
	h := newHarness(t)
	h.vars["OAI_PLAN_PROXY_LOG_FORMAT"] = "yaml"
	h.vars["OAI_PLAN_PROXY_LISTEN"] = freeAddr(t)

	done := runAsync(context.Background(), []string{"serve"}, h.env())
	code := waitExit(t, done)

	if code != 1 || !strings.Contains(h.stderr.String(), "OAI_PLAN_PROXY_LOG_FORMAT") {
		t.Fatalf("exit code = %d, stderr = %q; want 1 naming OAI_PLAN_PROXY_LOG_FORMAT", code, h.stderr.String())
	}
}

func TestServeWarnsAtStartupWhenNoSignInIsStored(t *testing.T) {
	h := newHarness(t)
	h.vars["OAI_PLAN_PROXY_LOG_FORMAT"] = "json"

	serveAndProbe(t, h)

	recs := logRecords(t, h.stderr.String(), "sign-in needed")
	if len(recs) != 1 {
		t.Fatalf("sign-in needed records = %d, want 1; stderr:\n%s", len(recs), h.stderr.String())
	}
	hint, _ := recs[0]["hint"].(string)
	if recs[0]["level"] != "WARN" || recs[0]["login"] != "missing" ||
		!strings.Contains(hint, "oai-plan-proxy login") || !strings.Contains(hint, "import") {
		t.Fatalf("sign-in needed record = %v", recs[0])
	}
}

func TestServeLogsTextByDefault(t *testing.T) {
	h := newHarness(t)

	serveAndProbe(t, h)

	if !strings.Contains(h.stderr.String(), "level=INFO msg=listening addr=") {
		t.Fatalf("stderr = %q, want a text record for listening", h.stderr.String())
	}
}

func TestServeLogsEachV1Request(t *testing.T) {
	h := newHarness(t)
	h.vars["OAI_PLAN_PROXY_LOG_FORMAT"] = "json"
	addr := freeAddr(t)
	h.vars["OAI_PLAN_PROXY_LISTEN"] = addr
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, []string{"serve"}, h.env())
	waitForGet(t, "http://"+addr+"/healthz").Body.Close()

	resp, err := http.Get("http://" + addr + "/v1/unknown")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	stopServe(t, cancel, done)

	var v1 []map[string]any
	for _, rec := range logRecords(t, h.stderr.String(), "request") {
		if rec["path"] == "/v1/unknown" {
			v1 = append(v1, rec)
		}
	}
	if len(v1) != 1 || v1[0]["status"] != float64(404) {
		t.Fatalf("/v1/unknown records = %v; stderr:\n%s", v1, h.stderr.String())
	}
}

func TestServeLogsAScheduledTokenRefresh(t *testing.T) {
	h := newHarness(t)
	h.fakeAuth(t)
	h.vars["OAI_PLAN_PROXY_LOG_FORMAT"] = "json"
	creds := signedIn()
	creds.ExpiresAt = time.Now().Add(30 * time.Second)
	h.saveCredentials(t, creds)
	h.vars["OAI_PLAN_PROXY_LISTEN"] = freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, []string{"serve"}, h.env())

	st, _ := store.Open(h.vars["OAI_PLAN_PROXY_DATA_DIR"])
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := st.Load(); err == nil && c.RefreshToken == "refresh-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never refreshed the sign-in")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	waitExit(t, done)

	recs := logRecords(t, h.stderr.String(), "token refreshed")
	if len(recs) != 1 || recs[0]["reason"] != "scheduled" {
		t.Fatalf("token refreshed records = %v; stderr:\n%s", recs, h.stderr.String())
	}
}
