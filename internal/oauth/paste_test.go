package oauth_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/oauth"
)

// pasteLogin runs Login with a pipe as its input and a token server that
// accepts the issued client. Call authURL after start to get the printed URL.
type pasteLogin struct {
	tokens  *fakeTokenServer
	out     *urlCatcher
	in      *io.PipeWriter
	stderr  *safeBuffer
	done    chan pasteOutcome
	authURL string
	// failExchange makes the token endpoint reject the code.
	failExchange atomic.Bool
}

type pasteOutcome struct {
	res oauth.Result
	err error
}

// safeBuffer is a goroutine-safe buffer for Login's error output.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startPasteLogin(t *testing.T) *pasteLogin {
	t.Helper()
	l := &pasteLogin{out: newURLCatcher(t), stderr: &safeBuffer{}, done: make(chan pasteOutcome, 1)}
	l.tokens = newFakeTokenServer(t, func(w http.ResponseWriter, r *http.Request) {
		if l.failExchange.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		claims := map[string]any{
			"iss": testEndpoints.Issuer, "aud": "oaiapp_issued", "sub": "user-sub-1",
			"email": "user@example.com", "nonce": query(t, l.authURL).Get("nonce"),
			"exp": time.Now().Add(time.Hour).Unix(),
		}
		writeTokens(w, unsignedJWT(t, claims))
	})
	inR, inW := io.Pipe()
	l.in = inW
	t.Cleanup(func() { inW.Close() })
	p := firstRegistration()
	p.Port = 0
	go func() {
		res, err := oauth.Login(context.Background(), oauth.LoginOptions{
			Endpoints: l.tokens.endpoints(), Params: p, Out: l.out.w, Client: l.tokens.Client(),
			In: inR, Err: l.stderr,
		})
		l.done <- pasteOutcome{res, err}
	}()
	l.authURL = l.out.next(t)
	return l
}

// callbackURL is the address the browser lands on after consent.
func (l *pasteLogin) callbackURL(t *testing.T, state string) string {
	t.Helper()
	q := query(t, l.authURL)
	return q.Get("redirect_uri") + "?" + url.Values{
		"code": {"code-1"}, "state": {state}, "client_id": {"oaiapp_issued"},
	}.Encode()
}

func (l *pasteLogin) paste(t *testing.T, line string) {
	t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(l.in, line+"\n")
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Login never read the pasted line")
	}
}

func (l *pasteLogin) wait(t *testing.T) pasteOutcome {
	t.Helper()
	select {
	case o := <-l.done:
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("Login did not return")
		return pasteOutcome{}
	}
}

func TestAPastedCallbackURLCompletesTheSignIn(t *testing.T) {
	l := startPasteLogin(t)

	l.paste(t, "  "+l.callbackURL(t, query(t, l.authURL).Get("state"))+"  ")

	o := l.wait(t)
	if o.err != nil {
		t.Fatal(o.err)
	}
	if o.res.ClientID != "oaiapp_issued" || o.res.RefreshToken != "refresh-1" {
		t.Fatalf("result = %+v, want the issued client and its tokens", o.res)
	}
}

func TestAPasteAfterTheBrowserCallbackIsIgnored(t *testing.T) {
	l := startPasteLogin(t)
	callback := l.callbackURL(t, query(t, l.authURL).Get("state"))
	resp, err := http.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if o := l.wait(t); o.err != nil {
		t.Fatal(o.err)
	}

	l.paste(t, callback)
	// The reader takes the next line only after it handled the previous
	// one, so a second write returning means the first paste was handled.
	l.paste(t, "")

	if n := l.tokens.requests.Load(); n != 1 {
		t.Fatalf("token requests = %d, want 1: the paste after completion must not exchange the code again", n)
	}
}

func TestTheListenerStillWorksAfterInputEnds(t *testing.T) {
	l := startPasteLogin(t)

	l.in.Close()
	resp, err := http.Get(l.callbackURL(t, query(t, l.authURL).Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if o := l.wait(t); o.err != nil {
		t.Fatalf("Login ended with %v, want the browser callback to complete it", o.err)
	}
}

func TestALineThatIsNotACallbackURLIsRejectedAndLoginKeepsWaiting(t *testing.T) {
	for _, line := range []string{"not-a-callback-secretish", "http://[::1", "http://127.0.0.1:1455/auth/callback?code=only"} {
		t.Run(line, func(t *testing.T) {
			l := startPasteLogin(t)

			l.paste(t, line)
			l.paste(t, l.callbackURL(t, query(t, l.authURL).Get("state")))

			if o := l.wait(t); o.err != nil {
				t.Fatalf("Login ended with %v, want the second paste to complete it", o.err)
			}
			if n := l.tokens.requests.Load(); n != 1 {
				t.Fatalf("token requests = %d, want 1", n)
			}
			errOut := l.stderr.String()
			if !strings.Contains(errOut, "paste the full address") || strings.Contains(errOut, line) {
				t.Fatalf("stderr = %q, want a hint without the rejected line", errOut)
			}
		})
	}
}

func TestAPastedURLWithAnotherStateIsRejectedAndLoginKeepsWaiting(t *testing.T) {
	l := startPasteLogin(t)

	l.paste(t, l.callbackURL(t, "state-from-an-older-sign-in"))
	l.paste(t, l.callbackURL(t, query(t, l.authURL).Get("state")))

	if o := l.wait(t); o.err != nil {
		t.Fatalf("Login ended with %v, want the second paste to complete it", o.err)
	}
	if n := l.tokens.requests.Load(); n != 1 {
		t.Fatalf("token requests = %d, want 1", n)
	}
	if errOut := l.stderr.String(); !strings.Contains(errOut, "unexpected state") {
		t.Fatalf("stderr = %q, want the unexpected-state message", errOut)
	}
}

func TestAPastedDeclinedConsentEndsTheSignIn(t *testing.T) {
	l := startPasteLogin(t)
	declined := query(t, l.authURL).Get("redirect_uri") + "?" + url.Values{
		"error": {"access_denied"}, "state": {query(t, l.authURL).Get("state")},
	}.Encode()

	l.paste(t, declined)

	o := l.wait(t)
	if o.err == nil || !strings.Contains(o.err.Error(), "sign-in was not completed: access_denied") {
		t.Fatalf("err = %v, want the declined sign-in", o.err)
	}
	if n := l.tokens.requests.Load(); n != 0 {
		t.Fatalf("token requests = %d, want 0", n)
	}
}

func TestAPastedCallbackWhoseExchangeFailsEndsTheSignIn(t *testing.T) {
	l := startPasteLogin(t)
	l.failExchange.Store(true)

	l.paste(t, l.callbackURL(t, query(t, l.authURL).Get("state")))

	o := l.wait(t)
	if o.err == nil || !strings.Contains(o.err.Error(), "invalid_grant") {
		t.Fatalf("err = %v, want the failed exchange", o.err)
	}
}
