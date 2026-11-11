package handler

import "time"

// The integration tests seed the mock runtime directly and share one Handler
// across tests, so a cached container read would leak one test's helper into
// the next. Disabling the TTL keeps every request honest; the cache itself is
// covered by update_probe_test.go.
func init() { updateProbeTTL = 0 }

// SetAppActionWait shortens how long Stop/Start/Restart wait before answering 202, for a test.
func SetAppActionWait(d time.Duration) (restore func()) {
	old := appActionWait
	appActionWait = d
	return func() { appActionWait = old }
}
