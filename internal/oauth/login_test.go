package oauth_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/oauth"
)

// urlCatcher captures the authorization URL that Login prints.
type urlCatcher struct {
	w    *io.PipeWriter
	urls chan string
}

func newURLCatcher(t *testing.T) *urlCatcher {
	r, w := io.Pipe()
	c := &urlCatcher{w: w, urls: make(chan string, 1)}
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "https://") {
				c.urls <- line
			}
		}
	}()
	t.Cleanup(func() { w.Close() })
	return c
}

func (c *urlCatcher) next(t *testing.T) string {
	t.Helper()
	select {
	case u := <-c.urls:
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("Login never printed an authorization URL")
		return ""
	}
}

func TestLoginStopsWaitingWhenCancelled(t *testing.T) {
	out := newURLCatcher(t)
	p := firstRegistration()
	p.Port = 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := oauth.Login(ctx, oauth.LoginOptions{
			Endpoints: testEndpoints, Params: p, Out: out.w, Client: http.DefaultClient,
		})
		done <- err
	}()
	out.next(t)

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Login returned no error after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Login kept waiting after cancel")
	}
}

func TestLoginCompletesWhenTheBrowserReturnsToTheLoopbackCallback(t *testing.T) {
	var authURL string
	tokens := newFakeTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		claims := map[string]any{
			"iss": testEndpoints.Issuer, "aud": "oaiapp_issued", "sub": "user-sub-1",
			"email": "user@example.com", "nonce": query(t, authURL).Get("nonce"),
			"exp": time.Now().Add(time.Hour).Unix(),
		}
		writeTokens(w, unsignedJWT(t, claims))
	})
	out := newURLCatcher(t)
	p := firstRegistration()
	p.Port = 0
	type outcome struct {
		res oauth.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := oauth.Login(context.Background(), oauth.LoginOptions{
			Endpoints: tokens.endpoints(), Params: p, Out: out.w, Client: tokens.Client(),
		})
		done <- outcome{res, err}
	}()

	authURL = out.next(t)
	q := query(t, authURL)
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		t.Fatal(err)
	}
	if redirect.Hostname() != "127.0.0.1" || redirect.Path != "/auth/callback" || redirect.Port() == "0" {
		t.Fatalf("redirect_uri = %q, want 127.0.0.1 on the bound port with /auth/callback", redirect)
	}
	browser := redirect.String() + "?" + url.Values{
		"code": {"code-1"}, "state": {q.Get("state")}, "client_id": {"oaiapp_issued"},
	}.Encode()
	resp, err := http.Get(browser)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatal(o.err)
		}
		if o.res.ClientID != "oaiapp_issued" || o.res.RefreshToken != "refresh-1" {
			t.Fatalf("result = %+v, want the issued client and its tokens", o.res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Login did not return after the callback")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("browser page status = %d, want 200", resp.StatusCode)
	}
}
