package waf

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RedactLogs makes Caddy's log text safe to show an admin.
//
// A rule-match line is the module's own formatting of the match and includes the
// matched payload and the request URI. Those lines have to stay in Caddy's stderr,
// because that is where the recorder reads them, so the place to stop them is
// where they are displayed. The Maintenance panel serves the tail of the
// container's log verbatim, and without this a form field the application's own
// users typed would be readable there.
//
// It does not blacklist the dangerous fields. A line that parses is rebuilt from
// the same whitelist the recorder stores, and a line that claims to be a rule
// match but does not parse is withheld whole, because a line whose grammar does
// not hold may have had text injected into it. Every other line is returned
// untouched, byte for byte.
func RedactLogs(content string) string {
	if !strings.Contains(content, LoggerName) {
		return content // the common case: no WAF, or nothing from it
	}
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		lines[i] = redactLine(l)
	}
	return strings.Join(lines, "\n")
}

func redactLine(line string) string {
	if !strings.Contains(line, `"logger":"`+LoggerName+`"`) {
		return line
	}
	ev, out := ParseCaddyLine(line)
	switch out {
	case Matched:
		return rebuild(line, summary(ev))
	case Malformed:
		return rebuild(line, "WAF rule match withheld: the line did not have the expected format")
	}

	// Not a rule match. The engine's own messages (a WAF instance reused, a rule
	// that failed to load) carry nothing from a request; anything that looks like it
	// does is withheld rather than trusted.
	for _, marker := range []string{`[client `, `[data `, `[uri `, `Coraza: `} {
		if strings.Contains(line, marker) {
			return rebuild(line, "WAF line withheld: it looks like it carries request data")
		}
	}
	return line
}

// summary is the safe account of a rule match.
func summary(ev Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "WAF rule %d (%s) matched: %s", ev.RuleID, ev.Severity, ev.Message)
	fmt.Fprintf(&b, "; client %s", ev.ClientIP)
	if ev.Host != "" {
		fmt.Fprintf(&b, "; host %s", ev.Host)
	}
	b.WriteString("; payload and URI withheld")
	return b.String()
}

// rebuild writes the line back as Caddy would, with msg replaced and every other
// field of the original kept. A line that cannot be read as JSON is replaced by a
// minimal one rather than passed through.
func rebuild(line, msg string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		fields = map[string]json.RawMessage{}
	}
	fields["logger"] = json.RawMessage(`"` + LoggerName + `"`)
	m, _ := json.Marshal(msg)
	fields["msg"] = m

	// Only fields Belune already trusts survive; the rest of the original line may
	// carry request data (the engine's debug lines add structured fields).
	safe := make(map[string]json.RawMessage, 4)
	for _, k := range []string{"level", "ts", "logger", "msg"} {
		if v, ok := fields[k]; ok {
			safe[k] = v
		}
	}
	out, err := json.Marshal(safe)
	if err != nil {
		return `{"logger":"` + LoggerName + `","msg":"WAF line withheld"}`
	}
	return string(out)
}
