package twitter

import (
	"os"
	"testing"
)

// TestMain keeps the process-wide queryId refresher OFF for the whole test
// binary: most tests build authenticated clients whose first request would
// otherwise start a background refresh against the real x.com. The refresh's own
// tests install an enabled refresher with useQIDRefresher.
func TestMain(m *testing.M) {
	processQIDPtr.Store(newQIDRefresher(qidConfig{disabled: true}))
	os.Exit(m.Run())
}
