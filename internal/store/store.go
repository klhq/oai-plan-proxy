// Package store keeps the proxy's credentials and host identity on disk.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klhq/oai-plan-proxy/internal/auth"
	"github.com/klhq/oai-plan-proxy/internal/oauth"
)

const (
	dirMode  = 0o700
	fileMode = 0o600

	hostIDFile      = "host_id"
	credentialsFile = "credentials.json"
)

// ErrNoCredentials means no sign-in has been stored.
var ErrNoCredentials = errors.New("no stored credentials")

// Credentials is one sign-in: the issued client ID and its tokens.
type Credentials struct {
	ClientID     string    `json:"client_id"`
	AccessToken  string    `json:"access_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
	Email        string    `json:"email,omitempty"`
}

// Load reads the stored credentials.
func (s *Store) Load() (Credentials, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, credentialsFile))
	if errors.Is(err, os.ErrNotExist) {
		return Credentials{}, ErrNoCredentials
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("read credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return Credentials{}, fmt.Errorf("parse credentials: %w", err)
	}
	return c, nil
}

// State reports the stored sign-in state.
// A record that keeps the issued client ID but has no refresh token is a
// sign-in the authorization server rejected.
func (s *Store) State() auth.State {
	c, err := s.Load()
	if err != nil {
		return auth.StateMissing
	}
	if c.RefreshToken == "" {
		return auth.StateDead
	}
	return auth.StateValid
}

// Save replaces the stored credentials. It writes a synced temp file and
// renames it over credentials.json, so a crash leaves either the old file or
// the new one, never a partial write.
func (s *Store) Save(c Credentials) error {
	if c.ClientID == oauth.DynamicClientID {
		return fmt.Errorf("refusing to store %q as the client ID: save the issued client ID instead", oauth.DynamicClientID)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	tmp, err := writeTemp(s.dir, credentialsFile, append(b, '\n'))
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, credentialsFile)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace credentials: %w", err)
	}
	return syncDir(s.dir)
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open data directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync data directory: %w", err)
	}
	return nil
}

// Store is the proxy's data directory.
type Store struct {
	dir string
}

// Open prepares the data directory at dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	// MkdirAll leaves an existing directory alone, and a fresh Docker volume
	// is usually 0755.
	if err := os.Chmod(dir, dirMode); err != nil {
		return nil, fmt.Errorf("restrict data directory: %w", err)
	}
	return &Store{dir: dir}, nil
}

// HostID returns this host's ext_agent_host_id, creating it on first use.
// OpenAI requires the value to stay the same for the life of the host.
func (s *Store) HostID() (string, error) {
	path := filepath.Join(s.dir, hostIDFile)
	if id, err := readHostID(path); err == nil {
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate host id: %w", err)
	}
	tmp, err := writeTemp(s.dir, hostIDFile, []byte(hex.EncodeToString(raw)+"\n"))
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	// Link fails when another process created the file first. Its value wins,
	// so every process agrees on one host id.
	if err := os.Link(tmp, path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("save host id: %w", err)
	}
	return readHostID(path)
}

func readHostID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("host id file %s is empty", path)
	}
	return id, nil
}

// writeTemp writes data to a new synced file next to the final name and
// returns its path.
func writeTemp(dir, name string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp file for %s: %w", name, err)
	}
	path := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Chmod(fileMode); err != nil {
		return fail(err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	return path, nil
}
