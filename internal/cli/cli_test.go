package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/cli"
	"github.com/klhq/oai-plan-proxy/internal/oauth"
	"github.com/klhq/oai-plan-proxy/internal/store"
)

// fakeAuth answers token requests for the authorization URL that login
// prints, so the CLI can run a full sign-in.
func (h *harness) fakeAuth(t *testing.T) (authURLs <-chan string) {
	t.Helper()
	urls := make(chan string, 1)
	r, w := io.Pipe()
	h.stdoutTap = w
	var mu sync.Mutex
	var current string
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "https://") {
				mu.Lock()
				current = line
				mu.Unlock()
				urls <- line
			}
		}
	}()
	t.Cleanup(func() { w.Close() })

	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		mu.Lock()
		u, _ := url.Parse(current)
		mu.Unlock()
		claims, _ := json.Marshal(map[string]any{
			"iss": "https://auth.example.test", "aud": "oaiapp_issued", "sub": "user-sub-1",
			"email": "user@example.com", "nonce": u.Query().Get("nonce"),
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		enc := base64.RawURLEncoding
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{
			"access_token": "access-1", "refresh_token": "refresh-1", "token_type": "Bearer",
			"expires_in": 3600,
			"id_token":   enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(claims) + ".sig",
		})
	}))
	t.Cleanup(srv.Close)
	h.client = srv.Client()
	h.oauth = oauth.Endpoints{
		AuthorizeURL: "https://auth.example.test/api/accounts/authorize",
		TokenURL:     srv.URL + "/api/accounts/oauth/token",
		Issuer:       "https://auth.example.test",
		Resource:     "https://api.example.test/v1",
	}
	return urls
}

// visitCallback plays the browser returning from the consent page.
func visitCallback(t *testing.T, authURL string, extra url.Values) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{"code": {"code-1"}, "state": {u.Query().Get("state")}}
	for k, v := range extra {
		q[k] = v
	}
	resp, err := http.Get(u.Query().Get("redirect_uri") + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func runAsync(ctx context.Context, args []string, env cli.Env) <-chan int {
	done := make(chan int, 1)
	go func() { done <- cli.Run(ctx, args, env) }()
	return done
}

func waitExit(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("command did not finish")
		return -1
	}
}

func TestLoginRegistersAndStoresTheIssuedClient(t *testing.T) {
	h := newHarness(t)
	urls := h.fakeAuth(t)
	port := freePort(t)

	done := runAsync(context.Background(), []string{"login", "--port", port}, h.env())
	authURL := nextURL(t, urls, done)
	visitCallback(t, authURL, url.Values{"client_id": {"oaiapp_issued"}})
	code := waitExit(t, done)

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, h.stderr.String())
	}
	q := mustQuery(t, authURL)
	st, _ := store.Open(h.vars["OAI_PLAN_PROXY_DATA_DIR"])
	hostID, _ := st.HostID()
	if q.Get("ext_agent_host_id") != hostID {
		t.Errorf("ext_agent_host_id = %q, want the stored host id %q", q.Get("ext_agent_host_id"), hostID)
	}
	if q.Get("redirect_uri") != "http://127.0.0.1:"+port+"/auth/callback" {
		t.Errorf("redirect_uri = %q, want port %s", q.Get("redirect_uri"), port)
	}
	creds, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if creds.ClientID != "oaiapp_issued" || creds.RefreshToken != "refresh-1" || creds.Email != "user@example.com" {
		t.Fatalf("stored %+v, want the issued client, its tokens and email", creds)
	}
}

func TestLoginAfterADeadSignInReusesTheStoredClientID(t *testing.T) {
	h := newHarness(t)
	h.saveCredentials(t, store.Credentials{ClientID: "oaiapp_issued"})
	urls := h.fakeAuth(t)

	done := runAsync(context.Background(), []string{"login", "--port", freePort(t)}, h.env())
	authURL := nextURL(t, urls, done)
	visitCallback(t, authURL, nil)
	code := waitExit(t, done)

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, h.stderr.String())
	}
	q := mustQuery(t, authURL)
	if q.Get("client_id") != "oaiapp_issued" || q.Has("agent_name_hint") {
		t.Fatalf("client_id = %q, agent_name_hint present = %v; want the stored ID and no hint",
			q.Get("client_id"), q.Has("agent_name_hint"))
	}
}

// nextURL waits for the printed authorization URL, failing fast when the
// command exits first.
func nextURL(t *testing.T, urls <-chan string, done <-chan int) string {
	t.Helper()
	select {
	case u := <-urls:
		return u
	case code := <-done:
		t.Fatalf("command exited with %d before printing an authorization URL", code)
	case <-time.After(5 * time.Second):
		t.Fatal("command never printed an authorization URL")
	}
	return ""
}

func freePort(t *testing.T) string {
	_, port, _ := net.SplitHostPort(freeAddr(t))
	return port
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// waitForGet polls url until the server answers or the deadline passes.
func waitForGet(t *testing.T, url string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never answered %s: %v", url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type harness struct {
	stdout, stderr syncBuffer
	vars           map[string]string
	oauth          oauth.Endpoints
	client         *http.Client
	// stdoutTap also receives stdout, so a test can read it while a command
	// is still running.
	stdoutTap io.Writer
}

// syncBuffer is a bytes.Buffer safe for a command goroutine and a test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newHarness(t *testing.T) *harness {
	return &harness{vars: map[string]string{"OAI_PLAN_PROXY_DATA_DIR": t.TempDir()}}
}

func (h *harness) env() cli.Env {
	return cli.Env{
		Stdout: h.out(),
		Stderr: &h.stderr,
		Getenv: func(k string) string { return h.vars[k] },
		UserConfigDir: func() (string, error) {
			panic("tests set OAI_PLAN_PROXY_DATA_DIR")
		},
		OAuth:      h.oauth,
		HTTPClient: h.client,
	}
}

func (h *harness) out() io.Writer {
	if h.stdoutTap != nil {
		return io.MultiWriter(&h.stdout, h.stdoutTap)
	}
	return &h.stdout
}

func TestUnknownCommandPrintsUsageAndExits2(t *testing.T) {
	h := newHarness(t)

	code := cli.Run(context.Background(), []string{"frobnicate"}, h.env())

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(h.stderr.String(), "usage") {
		t.Fatalf("stderr = %q, want usage text", h.stderr.String())
	}
}

func (h *harness) saveCredentials(t *testing.T, c store.Credentials) {
	t.Helper()
	s, err := store.Open(h.vars["OAI_PLAN_PROXY_DATA_DIR"])
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
}

func signedIn() store.Credentials {
	return store.Credentials{
		ClientID:     "oaiapp_test",
		AccessToken:  "secret-access-token",
		RefreshToken: "secret-refresh-token",
		IDToken:      "secret-id-token",
		ExpiresAt:    time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC),
		Email:        "user@example.com",
	}
}

func TestStatusPrintsTheSignInWithoutTokens(t *testing.T) {
	h := newHarness(t)
	h.saveCredentials(t, signedIn())

	code := cli.Run(context.Background(), []string{"status"}, h.env())

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{"valid", "user@example.com", "oaiapp_test", "2026-11-21T13:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out+h.stderr.String(), "secret-") {
		t.Errorf("status printed a token:\n%s", out)
	}
}

func TestStatusWithoutSignInPointsToLogin(t *testing.T) {
	h := newHarness(t)

	code := cli.Run(context.Background(), []string{"status"}, h.env())

	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "missing") || !strings.Contains(out, "oai-plan-proxy login") {
		t.Fatalf("stdout = %q, want the missing state and a login hint", out)
	}
}

// serveAndProbe runs serve, reads GET /healthz once, stops the server, and
// returns the health response with serve's exit code.
func serveAndProbe(t *testing.T, h *harness) (status int, body map[string]string, code int) {
	t.Helper()
	addr := freeAddr(t)
	h.vars["OAI_PLAN_PROXY_LISTEN"] = addr
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		code = cli.Run(ctx, []string{"serve"}, h.env())
	}()

	resp := waitForGet(t, "http://"+addr+"/healthz")
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	cancel()
	wg.Wait()
	return resp.StatusCode, body, code
}

func TestServeRefreshesANearlyExpiredSignInWithoutTraffic(t *testing.T) {
	h := newHarness(t)
	h.fakeAuth(t)
	creds := signedIn()
	creds.ExpiresAt = time.Now().Add(30 * time.Second)
	h.saveCredentials(t, creds)
	h.vars["OAI_PLAN_PROXY_LISTEN"] = freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, []string{"serve"}, h.env())
	defer func() { cancel(); waitExit(t, done) }()

	st, _ := store.Open(h.vars["OAI_PLAN_PROXY_DATA_DIR"])
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := st.Load()
		if err == nil && c.RefreshToken == "refresh-1" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never refreshed the sign-in; stored refresh token is still %q", c.RefreshToken)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServeAnswersHealthzWithoutLoginAndStopsOnCancel(t *testing.T) {
	h := newHarness(t)

	status, body, code := serveAndProbe(t, h)

	if status != http.StatusServiceUnavailable || body["login"] != "missing" {
		t.Fatalf("healthz = %d %v, want 503 with login missing", status, body)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, h.stderr.String())
	}
}

func TestServeReportsAStoredSignInAsHealthy(t *testing.T) {
	h := newHarness(t)
	h.saveCredentials(t, signedIn())

	status, body, _ := serveAndProbe(t, h)

	if status != http.StatusOK || body["login"] != "valid" {
		t.Fatalf("healthz = %d %v, want 200 with login valid", status, body)
	}
}
