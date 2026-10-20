package tlsstatus_test

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/belune-hq/belune/internal/tlsstatus"
)

// The expiry warning threshold is declared twice: here, where it decides whether
// a domain's TLS status becomes `expiring`, and in the frontend, where it
// decides whether the date in the Expires column turns amber. Nothing links
// them. Change one and the badge and the date it sits next to start disagreeing
// — the row says "Expiring" while the date beside it reads as fine, or the
// reverse — and no build, type check, or test would have said a word.
//
// So say a word. This asserts the two numbers are the same number.
//
// The file read below sits outside this Go module, so `go test` does not track
// it as an input and will happily replay a cached PASS after it changes. CI runs
// with -count=1 for exactly this reason; do not remove that flag. Locally, run
// `go test -count=1 ./internal/tlsstatus/` if you have touched the frontend.
const webExpiryFile = "../../../web/src/lib/expiry.ts"

var webExpiryDays = regexp.MustCompile(`EXPIRY_WARNING_DAYS\s*=\s*(\d+)`)

func TestExpiryWarningMatchesFrontend(t *testing.T) {
	src, err := os.ReadFile(webExpiryFile)
	if err != nil {
		t.Fatalf("read %s: %v (has the frontend constant moved? this test is the "+
			"only thing keeping it in step with tlsstatus.ExpiryWarning)", webExpiryFile, err)
	}

	m := webExpiryDays.FindSubmatch(src)
	if m == nil {
		t.Fatalf("no EXPIRY_WARNING_DAYS found in %s; if it was renamed, update "+
			"this test rather than deleting it", webExpiryFile)
	}

	days, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("EXPIRY_WARNING_DAYS in %s is not a number: %q", webExpiryFile, m[1])
	}

	want := time.Duration(days) * 24 * time.Hour
	if tlsstatus.ExpiryWarning != want {
		t.Errorf(
			"expiry warning threshold disagrees across the stack:\n"+
				"  backend tlsstatus.ExpiryWarning = %s (a domain goes `expiring` here)\n"+
				"  frontend EXPIRY_WARNING_DAYS    = %d days (the Expires date goes amber here)\n"+
				"a certificate would be badged one way and dated the other. "+
				"Change both, in %s and internal/tlsstatus/probe.go.",
			tlsstatus.ExpiryWarning, days, webExpiryFile,
		)
	}
}
