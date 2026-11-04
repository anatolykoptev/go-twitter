# Session storage

Sessions (`auth_token` + `ct0`) are loaded at startup, re-saved whenever x.com rotates `ct0`, and replaced when a relogin succeeds. A failed relogin leaves the stored session and the account's credentials as they were; relogin never reads the stored session. By default they live in `<SessionDir>/<username>.json` (`FileSessionStore`, atomic writes, 0600). Set `ClientConfig.SessionStore` to keep them elsewhere, for example in the database of your credential service:

```go
type dbStore struct{ db *sql.DB }

func (s dbStore) Load(ctx context.Context, user string) (twitter.Session, error) {
    var ss twitter.Session
    err := s.db.QueryRowContext(ctx, `SELECT auth_token, ct0, saved_at FROM tw_sessions WHERE username=$1`, user).
        Scan(&ss.AuthToken, &ss.CT0, &ss.SavedAt)
    if errors.Is(err, sql.ErrNoRows) {
        return twitter.Session{}, twitter.ErrSessionNotFound
    }
    return ss, err
}
func (s dbStore) Save(ctx context.Context, user string, ss twitter.Session) error {
    _, err := s.db.ExecContext(ctx, `INSERT INTO tw_sessions (username, auth_token, ct0, saved_at) VALUES ($1,$2,$3,$4)
        ON CONFLICT (username) DO UPDATE SET auth_token=$2, ct0=$3, saved_at=$4`, user, ss.AuthToken, ss.CT0, ss.SavedAt)
    return err
}
func (s dbStore) Delete(ctx context.Context, user string) error {
    _, err := s.db.ExecContext(ctx, `DELETE FROM tw_sessions WHERE username=$1`, user)
    return err
}

c, err := twitter.NewClient(twitter.ClientConfig{Accounts: accs, SessionStore: dbStore{db}})
```

Contract: `Load` returns `ErrSessionNotFound` on a miss. Expiry is the store's job: the client applies no TTL, and `SessionTTL` only configures the default file store. Every call gets a context bounded to 5s, and a store error is logged (username only) but never fails an API request. Stores must be safe for concurrent use. Only credentials leased by the `*WithSocial` helpers bypass the store; static-config accounts still load the stored session first. A `Load` error other than `ErrSessionNotFound` is not treated as a miss: provided credentials are used as-is, and an account with none fails instead of logging in with its password.
