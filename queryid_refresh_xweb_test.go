package twitter

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/anatolykoptev/go-twitter/internal/gqlextract"
)

// x-web source for the runtime refresh (#119 phase 1). The fake X serves, next
// to the legacy ops chunk, an x-web chunk holding the Relay artifact of
// tweetNotificationsQuery under a rotated id.

const (
	xwNewID    = "XWebRotatedID_0001"
	xwChunkURL = testViteBase + "assets/tweet-notifications-T1.js"
	xwOldID    = "VcNhzcrF-pRo8nSftDQThA" // the id inside the saved fixture chunk
)

// xwWorld layers the x-web chunk over qidWorld.
type xwWorld struct {
	*qidWorld
	chunk []byte
}

func (x *xwWorld) DoCtx(ctx context.Context, m, u string, h map[string]string, b io.Reader) ([]byte, map[string]string, int, error) {
	if u == xwChunkURL {
		x.mu.Lock()
		x.hits[u]++
		body := x.chunk
		x.mu.Unlock()
		return body, nil, 200, nil
	}
	return x.qidWorld.DoCtx(ctx, m, u, h, b)
}

func xwFixtureChunk(t *testing.T, id string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("internal", "gqlextract", "testdata", "xweb", "tweet-notifications.js"))
	require.NoError(t, err)
	return bytes.ReplaceAll(b, []byte(xwOldID), []byte(id))
}

// newXWebClient wires a client over a world whose entry script links the x-web
// chunk (an `import("./assets/…")` the legacy walk ignores).
func newXWebClient(t *testing.T, chunk []byte) (*Client, *xwWorld) {
	t.Helper()
	w := &xwWorld{qidWorld: newQIDWorld(t), chunk: chunk}
	w.extraJS = `export const lazy=()=>import("./assets/tweet-notifications-T1.js");`
	w.start(t)
	c, _ := newPoolWaitClient(t, 1)
	c.warmDoerOverride = w
	c.xtidMgr.SetWarmPageHook(observeWarmPage)
	return c, w
}

func TestQIDRefresh_XWeb_FlagOnAppliesRotatedNativeID(t *testing.T) {
	r, _ := useQIDRefresher(t)
	r.cfg.xweb = true
	c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))

	_ = c.search(ctxTimeout(t))
	r.wg.Wait()

	require.NotNil(t, activeQID.Load(), "refresh must have applied")
	assert.Equal(t, xwNewID, endpointFor(tweetNotificationsOp).ID)
	assert.Equal(t, Endpoints["SearchTimeline"].ID, endpointFor("SearchTimeline").ID, "legacy ops are untouched")
	assert.Equal(t, 1, w.hits[xwChunkURL])
}

// Default OFF: the x-web chunk is not even requested, and the id stays put.
//
// Mutation: in queryid_refresh.go build() make the `if r.cfg.xweb` guard
// unconditional => RED.
func TestQIDRefresh_XWeb_DefaultOffNeverCrawls(t *testing.T) {
	r, _ := useQIDRefresher(t)
	require.False(t, r.cfg.xweb, "the shipped default must be off")
	require.False(t, defaultQIDConfig().xweb)
	c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))

	_ = c.search(ctxTimeout(t))
	r.wg.Wait()

	assert.Zero(t, w.hits[xwChunkURL], "x-web chunk fetched with the flag off")
	assert.Equal(t, Endpoints[tweetNotificationsOp].ID, endpointFor(tweetNotificationsOp).ID)
}

func TestDefaultQIDConfig_XWebFollowsEnv(t *testing.T) {
	t.Setenv(envQIDXWeb, "on")
	assert.True(t, defaultQIDConfig().xweb)
	t.Setenv(envQIDXWeb, "")
	assert.False(t, defaultQIDConfig().xweb)
}

// An x-web-only bundle (the legacy walk finds none of the known ops) is still
// rejected as partial without the flag, and applies the native id with it.
//
// Mutation: in build() delete the `errors.Is(err, errQIDTooFew)` branch => RED.
func TestQIDRefresh_XWeb_OnlyBundleAppliesNativeIDAlone(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(map[bool]string{false: "flag off", true: "flag on"}[on], func(t *testing.T) {
			r, _ := useQIDRefresher(t)
			r.cfg.xweb = on
			c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))
			w.set(func(w *qidWorld) { w.advertised = map[string]string{} })

			_ = c.search(ctxTimeout(t))
			r.wg.Wait()

			if !on {
				assert.Nil(t, activeQID.Load(), "partial legacy bundle without x-web must change nothing")
				return
			}
			require.NotNil(t, activeQID.Load())
			assert.Equal(t, xwNewID, endpointFor(tweetNotificationsOp).ID)
			assert.Equal(t, Endpoints["SearchTimeline"].ID, endpointFor("SearchTimeline").ID)
		})
	}
}

// A malformed legacy id stays fatal even when x-web ids are available.
func TestQIDRefresh_XWeb_MalformedLegacyIDStillRejects(t *testing.T) {
	r, _ := useQIDRefresher(t)
	r.cfg.xweb = true
	c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))
	w.set(func(w *qidWorld) { w.extraJS += `{queryId:"BAD/ID",operationName:"UserTweets"},` })

	_ = c.search(ctxTimeout(t))
	r.wg.Wait()

	assert.Nil(t, activeQID.Load())
}

// Only native ops of the right kind take an x-web id; a mutation artifact
// carrying the native name is ignored.
func TestQIDRefresh_XWeb_WrongKindIgnored(t *testing.T) {
	r, _ := useQIDRefresher(t)
	r.cfg.xweb = true
	chunk := bytes.ReplaceAll(xwFixtureChunk(t, xwNewID), []byte("operationKind:`query`"), []byte("operationKind:`mutation`"))
	c, _ := newXWebClient(t, chunk)

	_ = c.search(ctxTimeout(t))
	r.wg.Wait()

	assert.Equal(t, Endpoints[tweetNotificationsOp].ID, endpointFor(tweetNotificationsOp).ID)
}

// A panic on a hostile x-web chunk must not take the legacy result down.
func TestQIDRefresh_XWeb_PanicKeepsLegacyRefresh(t *testing.T) {
	r, _ := useQIDRefresher(t)
	r.cfg.xweb = true
	orig := qidConsumeXWeb
	qidConsumeXWeb = func(*gqlextract.XWebExtractor, string, []byte) { panic("hostile chunk") }
	t.Cleanup(func() { qidConsumeXWeb = orig })
	c, w := newXWebClient(t, xwFixtureChunk(t, xwNewID))
	w.retire("SearchTimeline", qidNewSearch)

	_ = c.search(ctxTimeout(t))
	r.wg.Wait()

	require.NotNil(t, activeQID.Load(), "legacy refresh must survive an x-web panic")
	assert.Equal(t, qidNewSearch, endpointFor("SearchTimeline").ID)
	assert.Equal(t, Endpoints[tweetNotificationsOp].ID, endpointFor(tweetNotificationsOp).ID)
}
