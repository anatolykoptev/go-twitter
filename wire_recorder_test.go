package twitter

import (
	"sync"
	"testing"

	stealth "github.com/anatolykoptev/go-stealth"
)

// recordedReq is one request exactly as it was handed to the transport: the
// header map and the order list, before the transport sorts or encodes them.
type recordedReq struct {
	Method, URL string
	Headers     map[string]string
	Order       []string
}

// wireRecorder is a transport that records every request and answers a fixed
// body, so the headers a code path sends can be asserted without a network.
type wireRecorder struct {
	mu   sync.Mutex
	reqs []recordedReq
	cfg  stealth.BackendConfig   // the configuration of the last client built on this recorder (FetchMode = HTTP/2 weight 220)
	cfgs []stealth.BackendConfig // every client built on it, in order
}

// factory is the backend factory that hands the client this recorder and keeps
// the configuration it was built with.
func (r *wireRecorder) factory(cfg stealth.BackendConfig) (stealth.HTTPDoer, error) {
	r.mu.Lock()
	r.cfg = cfg
	r.cfgs = append(r.cfgs, cfg)
	r.mu.Unlock()
	return r, nil
}

func (r *wireRecorder) Do(req *stealth.Request) (*stealth.Response, error) {
	h := make(map[string]string, len(req.Headers))
	for k, v := range req.Headers {
		h[k] = v
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, recordedReq{
		Method: req.Method, URL: req.URL, Headers: h,
		Order: append([]string(nil), req.HeaderOrder...),
	})
	r.mu.Unlock()
	return &stealth.Response{
		StatusCode: 200,
		Body:       []byte(`{"guest_token":"gt-test","flow_token":"ft-test","subtasks":[]}`),
		Headers:    map[string]string{},
	}, nil
}

func (r *wireRecorder) SetProxy(string) error             { return nil }
func (r *wireRecorder) GetCookieValue(_, _ string) string { return "" }
func (r *wireRecorder) last() recordedReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqs[len(r.reqs)-1]
}

// recorderClient builds a client presenting profile whose transport is a
// wireRecorder.
func recorderClient(t *testing.T, p stealth.BrowserProfile) (*stealth.BrowserClient, *wireRecorder) {
	t.Helper()
	rec := &wireRecorder{}
	// Built the way the Client builds an account's client of this profile.
	opts := append([]stealth.ClientOption{
		stealth.WithIdentity(stealth.BrowserIdentity{BrowserProfile: p}),
		stealth.WithHeaderOrder(twitterHeaderOrder),
		stealth.WithBackend(rec.factory),
	}, wireClientOptions(wireGenFor(p.TLSProfile))...)
	bc, err := stealth.NewClient(opts...)
	if err != nil {
		t.Fatalf("recorder client: %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	return bc, rec
}
