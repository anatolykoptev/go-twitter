package twitter

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Calls() may be read while the request path still mutates the CallTrace.
//
// Mutation: trace.go CallTrace.update, delete the c.rt.mu.Lock()/Unlock() pair
// => `go test -race -run TestRequestTrace_ConcurrentReadIsRaceFree` reports a
// DATA RACE and fails.
func TestRequestTrace_ConcurrentReadIsRaceFree(t *testing.T) {
	rt := &RequestTrace{}
	ctx := WithRequestTrace(context.Background(), rt)
	ct := beginCall(ctx, "X")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			ct.jitterDone()
			ct.picked(i%2 == 0)
			ct.paced()
			ct.headersStart()
			ct.ready()
			ct.done(200, i)
		}
	}()
	for i := 0; i < 2000; i++ {
		_ = rt.Calls()
	}
	wg.Wait()

	calls := rt.Calls()
	assert.Len(t, calls, 1)
	assert.Equal(t, 2000, calls[0].Attempts)
	assert.False(t, calls[0].JitterDone.IsZero())
}

func TestCallTrace_NilSafe(t *testing.T) {
	var ct *CallTrace
	ct.jitterDone()
	ct.picked(true)
	ct.paced()
	ct.headersStart()
	ct.ready()
	ct.done(200, 1)
}
