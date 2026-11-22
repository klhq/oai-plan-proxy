package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/cli"
	"github.com/klhq/oai-plan-proxy/internal/store"
)

func TestExportedCredentialsInstallOnAnotherHost(t *testing.T) {
	laptop, server := newHarness(t), newHarness(t)
	laptop.saveCredentials(t, signedIn())
	file := filepath.Join(t.TempDir(), "creds.json")

	if code := cli.Run(context.Background(), []string{"export", file}, laptop.env()); code != 0 {
		t.Fatalf("export exit code = %d; stderr: %s", code, laptop.stderr.String())
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("export file mode = %o, want 600", mode)
	}
	if code := cli.Run(context.Background(), []string{"import", file}, server.env()); code != 0 {
		t.Fatalf("import exit code = %d; stderr: %s", code, server.stderr.String())
	}

	got, err := server.store(t).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got != signedIn() {
		t.Fatalf("imported %+v, want %+v", got, signedIn())
	}
}

func TestExportOverAWorldReadableFileMakesItPrivate(t *testing.T) {
	h := newHarness(t)
	h.saveCredentials(t, signedIn())
	file := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(file, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := cli.Run(context.Background(), []string{"export", file}, h.env()); code != 0 {
		t.Fatalf("export exit code = %d; stderr: %s", code, h.stderr.String())
	}

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("export file mode = %o, want 600", mode)
	}
}

// exportFile writes c to a file through export on a separate host.
func exportFile(t *testing.T, c store.Credentials) string {
	t.Helper()
	other := newHarness(t)
	other.saveCredentials(t, c)
	file := filepath.Join(t.TempDir(), "creds.json")
	if code := cli.Run(context.Background(), []string{"export", file}, other.env()); code != 0 {
		t.Fatalf("export exit code = %d; stderr: %s", code, other.stderr.String())
	}
	return file
}

func TestImportRefusesToReplaceExistingCredentialsWithoutForce(t *testing.T) {
	h := newHarness(t)
	h.saveCredentials(t, signedIn())
	incoming := signedIn()
	incoming.ClientID = "oaiapp_other"
	file := exportFile(t, incoming)

	code := cli.Run(context.Background(), []string{"import", file}, h.env())

	if code == 0 {
		t.Fatal("import exited 0 over existing credentials without --force")
	}
	if got, _ := h.store(t).Load(); got.ClientID != "oaiapp_test" {
		t.Fatalf("stored client ID = %q, want the original oaiapp_test kept", got.ClientID)
	}
	if !strings.Contains(h.stderr.String(), "--force") {
		t.Fatalf("stderr = %q, want a hint about --force", h.stderr.String())
	}
}

func TestImportWithForceReplacesExistingCredentials(t *testing.T) {
	h := newHarness(t)
	h.saveCredentials(t, signedIn())
	incoming := signedIn()
	incoming.ClientID = "oaiapp_other"
	file := exportFile(t, incoming)

	code := cli.Run(context.Background(), []string{"import", "--force", file}, h.env())

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, h.stderr.String())
	}
	if got, _ := h.store(t).Load(); got.ClientID != "oaiapp_other" {
		t.Fatalf("stored client ID = %q, want the imported oaiapp_other", got.ClientID)
	}
}

func TestImportRejectsAFileWithoutAUsableSignIn(t *testing.T) {
	h := newHarness(t)
	file := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(file, []byte(`{"OPENAI_API_KEY":null,"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code := cli.Run(context.Background(), []string{"import", file}, h.env())

	if code == 0 {
		t.Fatal("import exited 0 for a file with no client ID or refresh token")
	}
	if _, err := h.store(t).Load(); !errors.Is(err, store.ErrNoCredentials) {
		t.Fatalf("Load after a rejected import = %v, want ErrNoCredentials", err)
	}
}

func TestImportDashReadsTheCredentialsFromStandardInput(t *testing.T) {
	h := newHarness(t)
	b, err := os.ReadFile(exportFile(t, signedIn()))
	if err != nil {
		t.Fatal(err)
	}
	env := h.env()
	env.Stdin = bytes.NewReader(b)

	if code := cli.Run(context.Background(), []string{"import", "-"}, env); code != 0 {
		t.Fatalf("import - exit code = %d; stderr: %s", code, h.stderr.String())
	}

	if got, err := h.store(t).Load(); err != nil || got != signedIn() {
		t.Fatalf("stored %+v, %v; want %+v", got, err, signedIn())
	}
}
