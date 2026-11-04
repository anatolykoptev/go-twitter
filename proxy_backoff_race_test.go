package twitter

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anatolykoptev/go-twitter/xtid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// R4 — markProxyDown writes the backoff state while request goroutines pick
// accounts. Run under -race: every read of the backoff deadline in the pick
// filters must be synchronised with the write.
func TestProxyBackoff_ConcurrentMarkDownAndPicksAreRaceFree(t *testing.T) {
	c := newRotatingClient(t, ClientConfig{
		ProxyBackoffInitial: time.Microsecond,
		ProxyBackoffMax:     time.Microsecond,
	}, "u1:pw:tokA:ct0A")
	c.xtidMgr = xtid.NewManager(failFetcher{})
	acc := c.cfg.Accounts[0]

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				c.markProxyDown(acc)
			}
		}
	}()
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				_, _ = c.GetUserTweets(ctx, "999", 5) // outcome irrelevant: only the pick path matters
				cancel()
			}
		}()
	}
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = c.HealthReport()
	}
	close(stop)
	wg.Wait()
}

// markProxyDown's failure count and backoff deadline change together: no
// observer may see a count without the deadline that goes with it (#46).
func TestProxyBackoff_MarkDownIsOneCriticalSection(t *testing.T) {
	c := newRotatingClient(t, ClientConfig{
		ProxyBackoffInitial: time.Hour,
		ProxyBackoffMax:     time.Hour,
	}, "u1:pw:tokA:ct0A")
	acc := c.cfg.Accounts[0]

	stop := make(chan struct{})
	torn := make(chan [2]any, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			acc.mu.Lock()
			fails, until := acc.proxyConsecFails, acc.proxyBackoff
			acc.mu.Unlock()
			if fails > 0 && until.IsZero() {
				select {
				case torn <- [2]any{fails, until}:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 20000; i++ {
		acc.mu.Lock()
		acc.proxyConsecFails, acc.proxyBackoff = 0, time.Time{}
		acc.mu.Unlock()
		c.markProxyDown(acc)
		select {
		case <-torn:
			close(stop)
			wg.Wait()
			require.Fail(t, "observed a failure count without its backoff deadline")
		default:
		}
	}
	close(stop)
	wg.Wait()
	assert.NotZero(t, acc.proxyBackoff)
}
