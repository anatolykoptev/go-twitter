package twitter

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// F6 — credential-bearing malformed proxy rows must never reach logs, the
// alert payload or the NewClient error, whatever shape they take.
//
// Mutation: client.go proxyBuildFailure returns the raw proxy as label => RED.
func TestBadProxyClient_CredentialsNeverLeak(t *testing.T) {
	const secret = "SECRETPW"
	shapes := []string{
		"user:SECRETPW@host:8080",
		"http://user:SECRETPW@%zz",
		"socks5://user:SECRETPW@",
		"user:SECRETPW@[::1",
		"http://user:SECRETPW@ho st:8080",
	}
	for _, shape := range shapes {
		t.Run(shape, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			var payloads []string
			accs := ParseAccounts("leaky:pw:tokL:ct0L")
			accs[0].Proxy = shape
			c, err := NewClient(ClientConfig{
				Accounts:      accs,
				SessionDir:    t.TempDir(),
				skipPoolGauge: true,
				PoolAlertHook: func(topic string, payload any) {
					payloads = append(payloads, topic+" "+fmt.Sprintf("%v", payload))
				},
			})
			require.Error(t, err, "the proxy shape must fail to build (else this case proves nothing)")
			assert.Nil(t, c)
			require.NotEmpty(t, payloads, "the failure alert must have fired")
			assert.False(t, strings.Contains(err.Error(), secret), "NewClient error leaks credentials: %v", err)
			assert.False(t, strings.Contains(logs.String(), secret), "slog output leaks credentials: %s", logs.String())
			for _, p := range payloads {
				assert.False(t, strings.Contains(p, secret), "alert payload leaks credentials: %s", p)
			}
		})
	}
}
