// Package main implements the go-twitter #64 groundwork spike: a freshness
// comparison of X timeline sources (HomeLatestTimeline, ListLatestTweetsTimeline,
// SearchTimeline, UserTweets, and optionally notifications/device_follow.json).
//
// This file reads account credentials from the go-social Postgres store and
// disables/re-enables accounts for leasing during the measurement window.
// Credentials are decrypted in-process only; nothing secret is logged or
// written anywhere.
package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// socialAccount is one go-social accounts row with decrypted credentials.
// Secrets live here in memory only; they are never written to disk or logged.
type socialAccount struct {
	ID        string
	Username  string // accounts.username
	Status    string // status before the probe touched it
	Proxy     string // stored proxy URL (may lack a password; see WEBSHARE_PROXY_PASSWORD)
	Handle    string // credential-map "username" (screen name; falls back to Username)
	Password  string
	AuthToken string
	CT0       string
	TOTP      string
}

// psqlCmd is the docker binary used to reach the postgres container.
const psqlCmd = "docker"

// psql runs a read/write statement inside the postgres container as the memos
// superuser (peer auth on the local socket — no password on the command line).
// Output fields are unit-separator delimited, one row per line.
func psql(ctx context.Context, sql string) (string, error) {
	cmd := exec.CommandContext(ctx, psqlCmd,
		"exec", "postgres", "psql", "-U", "memos", "-d", "social",
		"-t", "-A", "-F", "\x1f", "-c", sql)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("psql: %w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return out.String(), nil
}

// loadEncryptionKey extracts SOCIAL_ENCRYPTION_KEY from the krolik-server .env
// file without printing it (grep-style single-key extraction).
func loadEncryptionKey(envPath string) ([]byte, error) {
	data, err := os.ReadFile(envPath)
	if err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	const prefix = "SOCIAL_ENCRYPTION_KEY="
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			v := strings.Trim(strings.TrimPrefix(line, prefix), `"'`)
			key, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return nil, fmt.Errorf("decode encryption key: %w", err)
			}
			if len(key) != 32 {
				return nil, fmt.Errorf("encryption key length %d, want 32", len(key))
			}
			return key, nil
		}
	}
	return nil, errors.New("SOCIAL_ENCRYPTION_KEY not found in env file")
}

// decryptAESGCM replicates go-social's internal/storage.Encryptor:
// ciphertext = [12-byte nonce][AES-256-GCM ciphertext+tag].
func decryptAESGCM(key, data []byte) ([]byte, error) {
	const nonceSize = 12
	if len(data) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
}

// quoteSQL escapes a string for a single-quoted SQL literal. Inputs here are
// operator-controlled usernames/UUIDs, but escape anyway.
func quoteSQL(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// loadAccounts fetches the named twitter accounts from go-social with full
// decrypted credentials (equivalent to GetInternalCredentials).
func loadAccounts(ctx context.Context, key []byte, usernames []string) (map[string]*socialAccount, error) {
	list := make([]string, 0, len(usernames))
	for _, u := range usernames {
		list = append(list, quoteSQL(u))
	}
	sql := `SELECT id, username, status, proxy, encode(credentials,'hex')
	        FROM accounts WHERE platform='twitter' AND username IN (` + strings.Join(list, ",") + `)`
	out, err := psql(ctx, sql)
	if err != nil {
		return nil, err
	}
	accs := make(map[string]*socialAccount, len(usernames))
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\x1f")
		if len(f) != 5 {
			return nil, fmt.Errorf("unexpected psql row (%d fields)", len(f))
		}
		raw, err := hex.DecodeString(f[4])
		if err != nil {
			return nil, fmt.Errorf("decode credentials hex for %s: %w", f[1], err)
		}
		plain, err := decryptAESGCM(key, raw)
		if err != nil {
			return nil, fmt.Errorf("decrypt credentials for %s: %w", f[1], err)
		}
		var creds map[string]string
		if err := json.Unmarshal(plain, &creds); err != nil {
			return nil, fmt.Errorf("unmarshal credentials for %s: %w", f[1], err)
		}
		a := &socialAccount{
			ID:        f[0],
			Username:  f[1],
			Status:    f[2],
			Proxy:     f[3],
			Handle:    creds["username"],
			Password:  creds["password"],
			AuthToken: creds["auth_token"],
			CT0:       creds["ct0"],
			TOTP:      creds["totp_secret"],
		}
		if a.Handle == "" {
			a.Handle = a.Username
		}
		accs[a.Username] = a
	}
	for _, u := range usernames {
		if _, ok := accs[u]; !ok {
			return nil, fmt.Errorf("twitter account %q not found in go-social", u)
		}
	}
	return accs, nil
}

// setAccountStatus sets the go-social accounts.status field, mirroring
// store.UpdateStatus's status write (consecutive_fails left untouched).
// Setting 'disabled' removes the account from leasing (pool.Acquire and the
// shared tweet client both skip disabled rows).
func setAccountStatus(ctx context.Context, id, status string) error {
	_, err := psql(ctx, fmt.Sprintf(
		`UPDATE accounts SET status=%s, updated_at=NOW() WHERE id=%s`, quoteSQL(status), quoteSQL(id)))
	return err
}
