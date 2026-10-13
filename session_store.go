package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrSessionNotFound is returned by SessionStore.Load when no session is stored
// for the username (or a store-side policy such as a TTL has expired it).
var ErrSessionNotFound = errors.New("twitter: session not found")

// errInvalidSessionUsername is returned for usernames that cannot be a single
// file name inside the session directory.
var errInvalidSessionUsername = errors.New("invalid session username")

// sessionStoreTimeout bounds every store call made on behalf of a request: a
// slow or hung store must never block an API call indefinitely.
const sessionStoreTimeout = 5 * time.Second

// Session is the persisted credential pair of one account. The JSON tags are
// the on-disk format of FileSessionStore and are kept stable.
//
// String and LogValue redact the tokens so a Session printed or logged by
// accident does not leak them; JSON marshalling is unaffected.
type Session struct {
	AuthToken string    `json:"auth_token"`
	CT0       string    `json:"ct0"`
	SavedAt   time.Time `json:"saved_at"`
}

// String implements fmt.Stringer with the tokens redacted.
func (s Session) String() string {
	return fmt.Sprintf("Session{AuthToken:[redacted] CT0:[redacted] SavedAt:%s}", s.SavedAt.Format(time.RFC3339))
}

// LogValue implements slog.LogValuer with the tokens redacted.
func (s Session) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("auth_token", "[redacted]"),
		slog.String("ct0", "[redacted]"),
		slog.Time("saved_at", s.SavedAt),
	)
}

// SessionStore persists per-account sessions (auth_token + ct0) across process
// restarts and ct0 rotations. Implementations must be safe for concurrent use.
//
// The client never lets a store failure fail an API request: errors are logged
// (username only, never tokens) and the request continues. Every call carries a
// context bounded to a few seconds.
//
// Implementations MUST honour ctx: the client bounds every call with a 5s
// timeout and calls Save synchronously on the request path right after a ct0
// rotation, so a store that ignores ctx stalls requests. Save is last-write-wins;
// the client sets Session.SavedAt (stores may compare it). Load must return
// ErrSessionNotFound for a miss; any other Load error is NOT treated as a miss:
// an account without provided credentials then fails instead of logging in
// with a password, so a flaky store cannot cause a login storm.
//
// Session lifetime is the store's concern: the client applies no TTL of its
// own. FileSessionStore honours ClientConfig.SessionTTL; a database-backed
// store can ignore Session.SavedAt entirely or enforce its own policy.
//
// Accounts leased by the *WithSocial helpers are credential-service owned and
// never touch the store.
type SessionStore interface {
	// Load returns the stored session, or ErrSessionNotFound when there is none.
	Load(ctx context.Context, username string) (Session, error)
	// Save stores the session, replacing any previous one.
	Save(ctx context.Context, username string, s Session) error
	// Delete removes the session; deleting a missing session is not an error.
	Delete(ctx context.Context, username string) error
}

// FileSessionStore keeps one JSON file per account at <Dir>/<username>.json.
// Writes are atomic (temp file + fsync + rename) so a concurrent Load sees the
// previous or the new complete file, never a truncated one. Files are 0600 in a
// 0700 directory.
type FileSessionStore struct {
	// Dir is the session directory. Empty means ~/.go-twitter/sessions.
	Dir string
	// TTL expires sessions older than this on Load (reported as
	// ErrSessionNotFound. TTL <= 0 means no expiry; note NewClient defaults
	// ClientConfig.SessionTTL 0 to 24h before building the default store.
	TTL time.Duration
}

var _ SessionStore = (*FileSessionStore)(nil)

func (f *FileSessionStore) dir() string {
	if f.Dir != "" {
		return f.Dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".go-twitter", "sessions")
}

// path returns the file path for a username. Usernames that are empty, contain a
// path separator or "..", or otherwise would not stay a single path element
// inside the directory are rejected: the name comes from a credential service
// and must never escape the session directory.
func (f *FileSessionStore) path(username string) (string, error) {
	if username == "" || strings.ContainsAny(username, `/\`+"\x00") ||
		strings.Contains(username, "..") || filepath.Base(username) != username {
		return "", fmt.Errorf("%w %q", errInvalidSessionUsername, username)
	}
	return filepath.Join(f.dir(), username+".json"), nil
}

// Save implements SessionStore. A zero SavedAt is stamped with the current time.
func (f *FileSessionStore) Save(_ context.Context, username string, s Session) error {
	path, err := f.path(username)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.dir(), 0700); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}
	if s.SavedAt.IsZero() {
		s.SavedAt = time.Now()
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, data, 0600); err != nil {
		return fmt.Errorf("write session %s: %w", path, err)
	}
	slog.Debug("session saved", slog.String("user", username))
	return nil
}

// Load implements SessionStore. A missing or TTL-expired file yields
// ErrSessionNotFound.
func (f *FileSessionStore) Load(_ context.Context, username string) (Session, error) {
	path, err := f.path(username)
	if err != nil {
		return Session{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return Session{}, err
	}
	if f.TTL > 0 && time.Since(s.SavedAt) > f.TTL {
		slog.Debug("session expired", slog.String("user", username))
		return Session{}, ErrSessionNotFound
	}
	return s, nil
}

// Delete implements SessionStore.
func (f *FileSessionStore) Delete(_ context.Context, username string) error {
	path, err := f.path(username)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// writeFileAtomic writes data to path via temp file + fsync + rename. The temp
// file lives in path's directory so the rename never crosses a filesystem.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// sessionStore returns the client's store, falling back to a FileSessionStore
// built from the config for hand-assembled Clients.
func (c *Client) sessionStore() SessionStore {
	if c.cfg.SessionStore != nil {
		return c.cfg.SessionStore
	}
	return &FileSessionStore{Dir: c.cfg.SessionDir, TTL: c.cfg.SessionTTL}
}

// sessionLoadedMsg keeps the historical "loaded session from disk" log line for
// the default file store (dashboards grep it); other stores say "store".
func (c *Client) sessionLoadedMsg() string {
	if _, ok := c.sessionStore().(*FileSessionStore); ok {
		return "loaded session from disk"
	}
	return "loaded session from store"
}

// loadSession returns the stored credentials for acc. A miss
// (ErrSessionNotFound) and leased accounts (which never touch the store) yield
// empty strings and a nil error. Any other store error is logged (username
// only) and returned: the stored state is unknown, which is not a miss.
func (c *Client) loadSession(acc *Account) (authToken, ct0 string, err error) {
	if acc.credsAuthoritative {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionStoreTimeout)
	defer cancel()
	s, err := c.sessionStore().Load(ctx, acc.Username)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return "", "", nil
		}
		slog.Warn("error loading session", slog.String("user", acc.Username), slog.Any("error", err))
		return "", "", err
	}
	return s.AuthToken, s.CT0, nil
}

// saveSession persists acc's credentials. Leased (credsAuthoritative) accounts
// are skipped. A store failure is logged and swallowed: it must not fail the
// request that triggered the save.
func (c *Client) saveSession(acc *Account, authToken, ct0 string) {
	if acc.credsAuthoritative {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionStoreTimeout)
	defer cancel()
	s := Session{AuthToken: authToken, CT0: ct0, SavedAt: time.Now()}
	if err := c.sessionStore().Save(ctx, acc.Username, s); err != nil {
		slog.Warn("session save failed", slog.String("user", acc.Username), slog.Any("error", err))
	}
}

// deleteSession removes acc's stored session (relogin). Leased accounts are
// skipped; failures are logged and returned.
func (c *Client) deleteSession(acc *Account) error {
	if acc.credsAuthoritative {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionStoreTimeout)
	defer cancel()
	if err := c.sessionStore().Delete(ctx, acc.Username); err != nil {
		slog.Warn("session delete failed", slog.String("user", acc.Username), slog.Any("error", err))
		return err
	}
	return nil
}
