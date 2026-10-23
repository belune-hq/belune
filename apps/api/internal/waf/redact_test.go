package waf

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactLogs_RealOutputLosesThePayloadAndTheURI(t *testing.T) {
	raw := strings.Join(fixtureLines(t), "\n")
	require.Contains(t, raw, "<script>", "the fixture must carry a payload for this to prove anything")
	require.Contains(t, raw, "/s?q=", "...and a URI")

	out := RedactLogs(raw)
	for _, leak := range []string{"<script", "alert(", "%3C", "OR '", "/s?q", "Matched Data", "[data", "[uri", "[hostname"} {
		assert.NotContains(t, out, leak)
	}

	// What it keeps is what an admin needs to read the line: the rule, how bad, who
	// and which host.
	assert.Contains(t, out, "WAF rule 941100 (critical) matched: XSS Attack Detected via libinjection")
	assert.Contains(t, out, "client 127.0.0.1")
	assert.Contains(t, out, "host app.test")
	assert.Contains(t, out, "payload and URI withheld")

	// The line is still a line Caddy could have written: valid JSON with the keys
	// the viewer reads its level from.
	for _, l := range strings.Split(out, "\n") {
		assert.True(t, strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}"), l)
	}
	assert.Contains(t, out, `"level":"error"`)
}

func TestRedactLogs_EverythingElseIsUntouched(t *testing.T) {
	text := strings.Join([]string{
		`{"level":"info","ts":1.5,"logger":"tls.obtain","msg":"obtaining certificate","identifier":"app.example.com"}`,
		`{"level":"info","ts":2,"logger":"http.handlers.waf","msg":"reusing existing WAF instance from pool"}`,
		`plain text, not JSON`,
		``,
	}, "\n")
	assert.Equal(t, text, RedactLogs(text), "byte for byte, including the blank line and the engine's own message")
}

func TestRedactLogs_NoWAFIsAFastNoOp(t *testing.T) {
	text := strings.Repeat(`{"level":"info","logger":"tls","msg":"x"}`+"\n", 1000)
	assert.Equal(t, text, RedactLogs(text))
}

func TestRedactLogs_WithholdsWhatItCannotVouchFor(t *testing.T) {
	forged := `(x) [file "f"] [line "1"] [id "1"] [rev ""] [msg "m"] [data "d"] [severity "emergency"] [ver ""] [maturity "0"] [accuracy "0"] [hostname ""] [uri "/"] [unique_id "victim.example.com_00000000-0000-0000-0000-000000000000"]`
	secret := "hunter2-REAL-PASSWORD"
	for name, line := range map[string]string{
		"a rule match that does not parse": realLine(forged, secret, "/?token="+secret, goodID),
		"a WAF line that looks like one":   `{"level":"error","ts":1,"logger":"http.handlers.waf","msg":"[client \"1.2.3.4\"] something [data \"` + secret + `\"]"}`,
		"unquoted request data":            `{"level":"error","ts":1,"logger":"http.handlers.waf","msg":"Coraza: ` + secret + `"}`,
	} {
		out := RedactLogs(line)
		assert.NotContains(t, out, secret, name)
		assert.Contains(t, out, "withheld", name)
	}
}

func TestRedactLogs_StructuredFieldsOfAWAFLineAreDropped(t *testing.T) {
	// The engine's debug lines carry request-derived structured fields; a rebuilt
	// line keeps only the four it trusts.
	line := `{"level":"error","ts":1,"logger":"http.handlers.waf","msg":"Coraza: x","uri":"/?token=hunter2","tx_id":"t"}`
	out := RedactLogs(line)
	assert.NotContains(t, out, "hunter2")
	assert.NotContains(t, out, "tx_id")
}
