package waf

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureLines are real lines from Caddy v2.11.7 with coraza-caddy v2.6.2 and CRS
// 4.25.0 (the published belune-caddy:2 build), captured from stderr while a
// cross-site-scripting and a SQL-injection payload were sent through a route that
// sets TxHeader. They are the grammar this parser has to read, not a guess at it.
func fixtureLines(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("testdata/caddy-coraza-crs-4.25.0.jsonl")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	require.NoError(t, sc.Err())
	return lines
}

func TestParseCaddyLine_RealCorazaOutput(t *testing.T) {
	var matched []Event
	notWAF := 0
	for _, l := range fixtureLines(t) {
		ev, out := ParseCaddyLine(l)
		switch out {
		case Matched:
			matched = append(matched, ev)
		case NotWAF:
			notWAF++
		default:
			t.Fatalf("a real line did not parse: %.200s", l)
		}
	}
	// Six rule matches (XSS x4, SQLi, the anomaly summary); the engine's own debug
	// lines and an unrelated logger are ignored, not miscounted as malformed.
	require.Len(t, matched, 6)
	assert.Equal(t, 3, notWAF)

	first := matched[0]
	assert.Equal(t, netip.MustParseAddr("127.0.0.1"), first.ClientIP)
	assert.Equal(t, 941100, first.RuleID)
	assert.Equal(t, "critical", first.Severity)
	assert.Equal(t, "attack-xss", first.Category)
	assert.Equal(t, "XSS Attack Detected via libinjection", first.Message)
	// The host asked for travels in the transaction id.
	assert.Equal(t, "app.test", first.Host)
	assert.Regexp(t, `^[0-9a-f-]{36}$`, first.TxID)
	assert.False(t, first.At.IsZero())

	// One request, several rules: they share a transaction id.
	for _, ev := range matched {
		assert.Equal(t, first.TxID, ev.TxID)
	}

	var summary *Event
	for i := range matched {
		if matched[i].RuleID == 949110 {
			summary = &matched[i]
		}
	}
	require.NotNil(t, summary, "the anomaly-score summary rule")
	assert.Equal(t, "Inbound Anomaly Score Exceeded", summary.Message, "the dynamic score is cut off")
}

// What a stored event must never contain. The fixtures were produced by requests
// whose payload and URI are known, so this is checkable rather than hoped for.
func TestParseCaddyLine_NeverKeepsThePayloadOrTheURI(t *testing.T) {
	for _, l := range fixtureLines(t) {
		ev, out := ParseCaddyLine(l)
		if out != Matched {
			continue
		}
		stored, err := json.Marshal(ev)
		require.NoError(t, err)
		for _, leak := range []string{"<script", "alert(", "OR '", "1=1", "/s?q", "%3C", "Matched Data", "[data", "[uri"} {
			assert.NotContains(t, string(stored), leak, "rule %d keeps %q", ev.RuleID, leak)
		}
	}
}

// msgFor builds a message in the exact shape Coraza writes, so a test can vary one
// part of it.
func msgFor(lead, data, uri, uniqueID string) string {
	return fmt.Sprintf(`[client "203.0.113.9"] Coraza: Warning. %s [file "f.conf"] [line "1"] [id "941100"] [rev ""] [msg "XSS Attack Detected via libinjection"] [data %q] [severity "critical"] [ver "OWASP_CRS/4.25.0"] [maturity "0"] [accuracy "0"] [tag "attack-xss"] [tag "OWASP_CRS"] [hostname ""] [uri %q] [unique_id %q]`,
		lead, data, uri, uniqueID)
}

// asLine wraps a message the way Caddy's JSON logger does.
func asLine(msg string) string {
	b, _ := json.Marshal(map[string]any{"level": "error", "ts": 1792742400.5, "logger": LoggerName, "msg": msg})
	return string(b)
}

func realLine(lead, data, uri, uniqueID string) string {
	return asLine(msgFor(lead, data, uri, uniqueID))
}

const goodID = "shop.example.com_8331db49-d28c-434d-be4a-7458091191b6"

func TestParseCaddyLine_AttackerTextCannotForgeARecord(t *testing.T) {
	t.Run("a baseline line parses", func(t *testing.T) {
		ev, out := ParseCaddyLine(realLine("XSS Attack Detected via libinjection", "d", "/", goodID))
		require.Equal(t, Matched, out)
		assert.Equal(t, "203.0.113.9", ev.ClientIP.String())
		assert.Equal(t, "shop.example.com", ev.Host)
	})

	t.Run("quoted values cannot spoof the fields after them", func(t *testing.T) {
		// The data is the attacker's. It tries to be a second record; Go-quoting
		// escapes its quotes, so it is only ever a string.
		evil := `x"] [client "198.51.100.7"] Coraza: Warning. y [file "f"] [line "1"] [id "1"] [severity "emergency"`
		ev, out := ParseCaddyLine(realLine("m", evil, "/?"+evil, goodID))
		require.Equal(t, Matched, out)
		assert.Equal(t, "203.0.113.9", ev.ClientIP.String(), "the client comes from the front of the line")
		assert.Equal(t, 941100, ev.RuleID)
		assert.Equal(t, "critical", ev.Severity)
	})

	t.Run("injected text in the unquoted message is rejected, not believed", func(t *testing.T) {
		// "HTTP header is restricted by policy (%{MATCHED_VAR})" puts a header value,
		// unquoted, in front of the real details. This one carries a complete forged
		// details block for another host.
		forged := `(x) [file "f"] [line "1"] [id "1"] [rev ""] [msg "m"] [data "d"] [severity "emergency"] [ver ""] [maturity "0"] [accuracy "0"] [hostname ""] [uri "/"] [unique_id "victim.example.com_00000000-0000-0000-0000-000000000000"]`
		_, out := ParseCaddyLine(realLine(forged, "d", "/", goodID))
		assert.Equal(t, Malformed, out, "the real block that follows the forged one is not part of the grammar")
	})

	t.Run("anything after the details is rejected", func(t *testing.T) {
		_, out := ParseCaddyLine(asLine(msgFor("m", "d", "/", goodID) + ` [unique_id "evil"]`))
		assert.Equal(t, Malformed, out)
	})

	t.Run("a truncated line is rejected", func(t *testing.T) {
		msg := msgFor("m", "d", "/", goodID)
		_, out := ParseCaddyLine(asLine(msg[:strings.Index(msg, `[severity`)]))
		assert.Equal(t, Malformed, out)
	})
}

func TestParseCaddyLine_RejectsWhatIsNotARecord(t *testing.T) {
	good := msgFor("m", "d", "/", goodID)
	cases := map[string]string{
		"client is not an address": strings.Replace(good, `203.0.113.9`, `not-an-ip`, 1),
		"client is unspecified":    strings.Replace(good, `203.0.113.9`, `0.0.0.0`, 1),
		"unknown severity word":    strings.Replace(good, `[severity "critical"]`, `[severity "apocalyptic"]`, 1),
		"rule id is not a number":  strings.Replace(good, `[id "941100"]`, `[id "abc"]`, 1),
		"a field out of order":     strings.Replace(good, `[line "1"] [id "941100"]`, `[id "941100"] [line "1"]`, 1),
	}
	for name, msg := range cases {
		require.NotEqual(t, good, msg, "%s: the case must change something", name)
		_, out := ParseCaddyLine(asLine(msg))
		assert.Equal(t, Malformed, out, name)
	}
	_, out := ParseCaddyLine(`{"logger":"http.handlers.waf" Coraza: broken`)
	assert.Equal(t, Malformed, out, "not JSON")

	// Coraza reports a rule that declares no severity as "unknown"; the CRS anomaly
	// summary is one, and it is the row that says a request would have been blocked.
	ev, out := ParseCaddyLine(asLine(strings.Replace(good, `[severity "critical"]`, `[severity "unknown"]`, 1)))
	require.Equal(t, Matched, out)
	assert.Equal(t, "unknown", ev.Severity)
}

func TestParseCaddyLine_NormalisesTheClientAddress(t *testing.T) {
	for in, want := range map[string]string{
		"::ffff:203.0.113.9": "203.0.113.9", // an IPv4 client on a dual-stack listener
		"fe80::1%eth0":       "fe80::1",     // a zone is not part of an address
		"2001:db8::1":        "2001:db8::1",
	} {
		ev, out := ParseCaddyLine(asLine(strings.Replace(msgFor("m", "d", "/", goodID), "203.0.113.9", in, 1)))
		require.Equal(t, Matched, out, in)
		assert.Equal(t, want, ev.ClientIP.String(), in)
	}
}

func TestParseCaddyLine_IgnoresEverythingElse(t *testing.T) {
	for name, l := range map[string]string{
		"empty":                           "",
		"an access entry":                 `{"level":"info","ts":1.0,"logger":"http.log.access.http.access.srv0","msg":"handled request"}`,
		"another logger quoting this one": `{"level":"info","ts":1.0,"logger":"tls","msg":"saw \"logger\":\"http.handlers.waf\" and Coraza: text"}`,
		"the engine's own line":           `{"level":"warn","ts":1.0,"logger":"http.handlers.waf","msg":"Coraza: something internal"}`,
		"enforcement's line":              `{"level":"error","ts":1.0,"logger":"http.handlers.waf","msg":"WAF rule violation detected","hostname":"h","uri":"/x?token=secret"}`,
	} {
		_, out := ParseCaddyLine(l)
		assert.Equal(t, NotWAF, out, name)
	}
}

func TestSplitTxID(t *testing.T) {
	const u = "8331db49-d28c-434d-be4a-7458091191b6"
	for _, tc := range []struct{ name, id, host, tx string }{
		{"host and uuid", "shop.example.com_" + u, "shop.example.com", u},
		{"host is lower-cased", "Shop.Example.COM_" + u, "shop.example.com", u},
		{"a host may contain an underscore", "my_app.example.com_" + u, "my_app.example.com", u},
		{"Coraza's own random id carries no host", "dzZFiXVZIsVuPAtt", "", "dzZFiXVZIsVuPAtt"},
		// A host that is not believable is dropped, but the request id is ours and
		// random, so it still groups the rules one request tripped.
		{"a host that is not a hostname is not believed", "bad host_" + u, "", u},
		{"consecutive dots are not a hostname", "a..b_" + u, "", u},
		{"a trailing dot is not accepted", "shop.example.com._" + u, "", u},
		{"a non-uuid tail is not split", "shop.example.com_notauuid", "", "shop.example.com_notauuid"},
		{"empty", "", "", ""},
	} {
		h, tx := splitTxID(tc.id)
		assert.Equal(t, tc.host, h, tc.name)
		assert.Equal(t, tc.tx, tx, tc.name)
	}
}

func TestStaticMessage(t *testing.T) {
	assert.Equal(t, "HTTP header is restricted by policy", staticMessage("HTTP header is restricted by policy (Proxy: evil)"))
	assert.Equal(t, "HTTP Parameter Pollution", staticMessage("HTTP Parameter Pollution (ARGS_NAMES:secret_name)"))
	assert.Equal(t, "Inbound Anomaly Score Exceeded", staticMessage("Inbound Anomaly Score Exceeded (Total Score: 25)"))
	assert.Equal(t, "a?b", staticMessage("a\x00b"), "control characters are replaced, not stored")
	assert.Equal(t, "caf?", staticMessage("café"), "stored text is plain ASCII")
	assert.Len(t, staticMessage(strings.Repeat("x", 500)), maxMessage)
}

// The hook runs inline on the collector's read path for every line Caddy writes,
// and nearly all of them are not WAF lines, so that path has to be a substring
// check. This pins it: a regression that parsed JSON first would show up here.
func BenchmarkParseCaddyLine_NotWAF(b *testing.B) {
	line := `{"level":"info","ts":1792742400.1954758,"logger":"tls.obtain","msg":"obtaining certificate","identifier":"app.example.com"}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, out := ParseCaddyLine(line); out != NotWAF {
			b.Fatal("misclassified")
		}
	}
}
