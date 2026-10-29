// Package waf turns Coraza rule matches, as Caddy logs them, into detection
// events.
//
// A rule match reaches Belune as one line of Caddy's stderr from the
// http.handlers.waf logger. That line is the module's own formatting of the match
// and carries request data Belune must not keep: [data "..."] is the matched
// payload and [uri "..."] the full request URI, both attacker-controlled and able
// to hold the user's own credentials. So nothing here searches the line for the
// fields it wants. It reads the fixed grammar from the start, rejects any line
// that does not parse completely, and copies out only a whitelist of fields.
package waf

import (
	"encoding/json"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LoggerName is the Caddy logger the module writes rule matches to.
const LoggerName = "http.handlers.waf"

// TxHeader carries the transaction id into the module (its tx_id_req_header). The
// proxy builds it as "<host>_<request uuid>", so the host that was asked for rides
// in the id Coraza logs as unique_id — no second pipeline has to be joined to learn
// it. It is set by Belune on every request, overwriting anything the client sent,
// and removed again before the request reaches the application.
const TxHeader = "X-Belune-Waf-Tx"

// Event is one rule match, reduced to what Belune keeps.
type Event struct {
	// At is when Caddy logged the match; zero when the line carried no usable time.
	At       time.Time
	ClientIP netip.Addr
	RuleID   int
	Severity string
	// Category is the rule's attack-* tag (attack-xss, attack-sqli, ...), if any.
	Category string
	// Message is the rule's static message, cut before any dynamic tail.
	Message string
	// Host is the host the request asked for; empty when the transaction id did not
	// carry one. An event without a host is still an event.
	Host string
	// TxID groups the rules one request tripped.
	TxID string
	// MatchedTarget is the request variable the rule matched in — a NAME, such
	// as "REQUEST_COOKIES:session" or "ARGS:q", never the value found in it.
	// Empty when the line did not say, which is normal for rules that match on
	// the request as a whole. See matchedTarget for what it is and is not.
	MatchedTarget string
	// Disposition is what Coraza did with the request: DispositionBlocked,
	// DispositionDetected, or empty when the line began with neither literal.
	// ⚠️ Per RULE LINE, not per request — see parseDisposition.
	Disposition string
	// AnomalyScore is the total the scoring rule reported, or 0 when this is not
	// that rule (or its message was not in the exact shape). 0 can stand for
	// "none" because that rule only fires at or above a threshold of at least 1,
	// and it keeps the zero Event meaning "no score".
	AnomalyScore int
}

// The two dispositions Belune stores, matching the CHECK on waf_events.
const (
	DispositionBlocked  = "blocked"
	DispositionDetected = "detected"
)

// Outcome says what ParseCaddyLine made of a line.
type Outcome int

const (
	// NotWAF is any line that is not a rule match: ignore it.
	NotWAF Outcome = iota
	// Malformed is a line that claims to be a rule match and does not parse
	// completely. It is counted, never stored: a line whose grammar does not hold
	// may have had text injected into it.
	Malformed
	// Matched is a parsed rule match.
	Matched
)

var (
	severities = map[string]struct{}{
		"emergency": {}, "alert": {}, "critical": {}, "error": {},
		"warning": {}, "notice": {}, "info": {}, "debug": {}, "unknown": {},
	}
	categoryRe = regexp.MustCompile(`^attack-[a-z0-9-]{1,40}$`)
	// The head of Coraza's data field: "Matched Data: <match> found within
	// <COLLECTION>[:<key>]: <value>". Both <match> and <value> are
	// attacker-controlled, so the capture groups are the collection and the key
	// and stop dead at the colon that introduces the value.
	//
	// The value is introduced by ": " in both forms — "REQUEST_URI: /x" and
	// "ARGS:q: 1=1" — so requiring that literal separator is what tells a
	// keyless target from a keyed one. The key group admits neither a space nor
	// a colon, so it cannot run into the value.
	targetRe = regexp.MustCompile(`found within ([A-Z_]{1,40})(?::([^\s:]{1,80}))?: `)
	uuidRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hostRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,252}$`)
	txRe     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// AnomalySummaryRule is the CRS rule that evaluates a request's accumulated
// anomaly score against the threshold — the one rule that states a verdict on the
// whole request rather than reporting a single match. It declares no severity, so
// Coraza reports it as "unknown"; the dashboard names it instead, and mirrors this
// number in waf-view.ts.
const AnomalySummaryRule = 949110

// maxMessage bounds a stored rule message; CRS messages are far shorter.
const maxMessage = 120

// maxTarget bounds a stored variable name, in bytes. A cookie or parameter name
// is client-supplied, so it is bounded here as well as by the pattern — see
// asciiBounded for why the pattern's own limit is not enough.
const maxTarget = 100

// fixedFields is the order Coraza writes the block that follows the free-text
// message, up to the tags.
var fixedFields = []string{"file", "line", "id", "rev", "msg", "data", "severity", "ver", "maturity", "accuracy"}

// ParseCaddyLine reads one line of Caddy's stderr.
//
// It is called inline on the log collector's read path for every line Caddy
// writes, so the common case — a line that is not a WAF line — is a substring
// check and nothing more.
func ParseCaddyLine(line string) (Event, Outcome) {
	if !strings.Contains(line, `"logger":"`+LoggerName+`"`) || !strings.Contains(line, "Coraza: ") {
		return Event{}, NotWAF
	}
	var entry struct {
		TS     float64 `json:"ts"`
		Logger string  `json:"logger"`
		Msg    string  `json:"msg"`
	}
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return Event{}, Malformed
	}
	if entry.Logger != LoggerName || !strings.HasPrefix(entry.Msg, "[client ") {
		return Event{}, NotWAF
	}

	ev, ok := parseMatch(entry.Msg)
	if !ok {
		return Event{}, Malformed
	}
	if entry.TS > 0 {
		sec := int64(entry.TS)
		ev.At = time.Unix(sec, int64((entry.TS-float64(sec))*1e9))
	}
	return ev, Matched
}

// parseMatch reads the whole of a rule-match message, or rejects it.
//
// The one stretch of free text in it — the rule message between "Coraza: Warning."
// and "[file" — is written unquoted, and CRS rules such as "HTTP header is
// restricted by policy (%{MATCHED_VAR})" expand request data into it. Everything
// else is Go-quoted, so a value cannot close its own quotes or spoof the field
// after it. Requiring the grammar to hold to the very end is what makes injected
// text in that one stretch useless: a forged block would be followed by the real
// one, and the real one is not part of the grammar.
func parseMatch(msg string) (Event, bool) {
	c := scanner{msg}

	client, ok := c.field("client")
	if !ok || !c.consume(" Coraza: ") {
		return Event{}, false
	}
	// The disposition and rule message, up to the details block.
	i := strings.Index(c.s, ` [file "`)
	if i < 0 {
		return Event{}, false
	}
	disposition := parseDisposition(c.s[:i])
	c.s = c.s[i+1:]

	values := make(map[string]string, len(fixedFields))
	for n, name := range fixedFields {
		v, ok := c.field(name)
		if !ok {
			return Event{}, false
		}
		values[name] = v
		if n < len(fixedFields)-1 && !c.consume(" ") {
			return Event{}, false
		}
	}

	var category string
	for strings.HasPrefix(c.s, " [tag ") {
		c.consume(" ")
		tag, ok := c.field("tag")
		if !ok {
			return Event{}, false
		}
		if category == "" && categoryRe.MatchString(tag) {
			category = tag
		}
	}

	// hostname and uri are read to prove the grammar, then discarded: the URI is
	// attacker-controlled and routinely carries a query string.
	var uniqueID string
	for _, name := range []string{"hostname", "uri", "unique_id"} {
		if !c.consume(" ") {
			return Event{}, false
		}
		v, ok := c.field(name)
		if !ok {
			return Event{}, false
		}
		if name == "unique_id" {
			uniqueID = v
		}
	}
	// A rule chain appends a msg_match_N/data_match_N pair per further match.
	for c.s != "" {
		if !c.extra("msg_match_") || !c.extra("data_match_") {
			return Event{}, false
		}
	}

	ip, err := netip.ParseAddr(client)
	if err != nil || !ip.IsValid() || ip.IsUnspecified() {
		return Event{}, false
	}
	ruleID, err := strconv.Atoi(values["id"])
	if err != nil || ruleID < 0 {
		return Event{}, false
	}
	severity := strings.ToLower(values["severity"])
	if _, ok := severities[severity]; !ok {
		return Event{}, false
	}

	host, tx := splitTxID(uniqueID)
	return Event{
		ClientIP:      ip.WithZone("").Unmap(),
		RuleID:        ruleID,
		Severity:      severity,
		Category:      category,
		Message:       staticMessage(values["msg"]),
		Host:          host,
		TxID:          tx,
		MatchedTarget: matchedTarget(values["data"]),
		Disposition:   disposition,
		AnomalyScore:  anomalyScore(ruleID, values["msg"]),
	}, true
}

// dispositionLiterals is the closed set of openings Coraza gives a rule line:
// "Access denied (phase N)." when the rule denies the request, "Warning." when
// it only reports. Observed levels run the "wrong" way round — a denial logs at
// warn and a report at error — so the level is no evidence and is not read.
var dispositionLiterals = []struct{ prefix, disposition string }{
	{"Access denied (phase 1).", DispositionBlocked},
	{"Access denied (phase 2).", DispositionBlocked},
	{"Access denied (phase 3).", DispositionBlocked},
	{"Access denied (phase 4).", DispositionBlocked},
	{"Access denied (phase 5).", DispositionBlocked},
	{"Warning.", DispositionDetected},
}

// parseDisposition reads the head of the free-text stretch (everything between
// "Coraza: " and " [file") as a strict PREFIX against a closed set, and nothing
// else.
//
// ⛔⛔ Never a substring search. This stretch is the one unquoted region of the
// line and CRS expands request data into it, so a payload can put the words
// "Access denied" anywhere after the opening — and a Contains() would let it
// label its own detection "blocked". The opening is Coraza's, written before any
// rule data, which is the only reason a prefix can be believed. The literal must
// be followed by a space or the end of the stretch, so "Warning.x" is not
// "Warning.". Neither literal means "" (stored NULL), never a guess.
//
// ⚠️ It describes THIS LINE. A blocked request logs "Access denied" on its
// deny-ing rule only; the signature rules that fed the score logged "Warning."
// under the same transaction id. Whether a request was blocked is a question
// for the transaction, answered at read time.
func parseDisposition(head string) string {
	for _, l := range dispositionLiterals {
		if rest, ok := strings.CutPrefix(head, l.prefix); ok && (rest == "" || rest[0] == ' ') {
			return l.disposition
		}
	}
	return ""
}

// scoreRules are the CRS rules whose message carries the request's total score.
var scoreRules = map[int]*regexp.Regexp{
	949110: regexp.MustCompile(`^Inbound Anomaly Score Exceeded \(Total Score: (\d{1,6})\)$`),
	959100: regexp.MustCompile(`^Outbound Anomaly Score Exceeded \(Total Score: (\d{1,6})\)$`),
}

// anomalyScore reads the total out of the scoring rule's message, or returns 0.
//
// staticMessage cuts messages at " (" because that is where CRS appends request
// data, which is why the score was dropped. It is recovered here without
// loosening that: only the two scoring rules, only a message that is exactly
// their words and one bounded number, anchored at both ends. A general scan of
// the parenthesis would let any rule's expanded data supply a "score".
func anomalyScore(ruleID int, msg string) int {
	re, ok := scoreRules[ruleID]
	if !ok {
		return 0
	}
	m := re.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// matchedTarget names the request variable a rule matched in, from the data
// field the grammar above has already read and would otherwise discard.
//
// ⛔ The NAME only. Everything after the colon that introduces the value stays
// out, because that is the matched payload — the thing 000070 exists to not
// keep. A name is a different class of data: "REQUEST_COOKIES:session" says
// where to look without saying what was in it, and that is the whole difference
// between a detection an operator can judge and one they cannot. Belune's own
// 920420 noise was diagnosed from exactly this field, read out of Caddy's
// container log by hand because it was not stored.
//
// ⚠️ It is a HINT, not evidence. The data field's head is the rule's own match
// output, which is attacker-controlled, so a payload containing the words this
// pattern looks for can forge the name. The pattern and maxTarget bound the
// damage to a wrong name of bounded length and charset; they cannot make it
// trustworthy. Nothing may key a decision on it.
func matchedTarget(data string) string {
	m := targetRe.FindStringSubmatch(data)
	if m == nil {
		return ""
	}
	target := m[1]
	if m[2] != "" {
		target += ":" + m[2]
	}
	// The pattern bounds the shape, not the bytes: the key group admits any rune
	// that is neither a space nor a colon, so a parameter name the request chose
	// can be multi-byte, or carry a NUL that survived the scanner's Unquote. Both
	// are stored verbatim without this, and neither is a value Postgres accepts.
	return asciiBounded(target, maxTarget)
}

// splitTxID separates "<host>_<uuid>". Coraza falls back to a random id of its own
// when the header is missing or invalid, which carries no host — that is a normal
// outcome, not an error. The host is client-influenced (a wildcard route matches
// any label), so it is validated before it is believed.
func splitTxID(id string) (host, tx string) {
	if i := strings.LastIndexByte(id, '_'); i > 0 {
		if t := id[i+1:]; uuidRe.MatchString(t) {
			// The uuid is ours and random, so it is kept for grouping even when the
			// host in front of it is not believable.
			if h := strings.ToLower(id[:i]); hostRe.MatchString(h) && !strings.Contains(h, "..") && !strings.HasSuffix(h, ".") {
				return h, t
			}
			return "", t
		}
	}
	if txRe.MatchString(id) {
		return "", id
	}
	return "", ""
}

// staticMessage keeps the part of a rule message that belongs to the rule. CRS
// messages that echo request data (MATCHED_VAR, a parameter name) all do it in a
// trailing parenthesis, so cutting there leaves the rule's own words and nothing
// the request supplied.
func staticMessage(msg string) string {
	if i := strings.Index(msg, " ("); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(asciiBounded(msg, maxMessage))
}

// asciiBounded renders s as printable ASCII within max bytes. Both halves are
// load-bearing for anything client-supplied that is then stored.
//
// ⚠️⚠️ The cap is checked BEFORE a rune is written, never by slicing after. A
// Go regexp quantifier counts RUNES while a slice index counts BYTES, so a
// pattern that admits 80 characters admits up to 320 bytes, and cutting that at
// a byte offset ends mid-rune. Postgres rejects invalid UTF-8 and NUL outright,
// and these rows are inserted with COPY — which fails whole, so one such value
// takes its entire batch of detections with it, at the moment the detections
// are the evidence. Filtering to ASCII is what bounds the charset; breaking
// before the write is what keeps the length cut on a character boundary.
func asciiBounded(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= max {
			break
		}
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// scanner consumes a string from the front.
type scanner struct{ s string }

func (c *scanner) consume(lit string) bool {
	if strings.HasPrefix(c.s, lit) {
		c.s = c.s[len(lit):]
		return true
	}
	return false
}

// quoted reads one Go-quoted string, which is how Coraza writes every value.
func (c *scanner) quoted() (string, bool) {
	q, err := strconv.QuotedPrefix(c.s)
	if err != nil {
		return "", false
	}
	v, err := strconv.Unquote(q)
	if err != nil {
		return "", false
	}
	c.s = c.s[len(q):]
	return v, true
}

// field reads `[name "value"]`.
func (c *scanner) field(name string) (string, bool) {
	if !c.consume("[" + name + " ") {
		return "", false
	}
	v, ok := c.quoted()
	if !ok || !c.consume("]") {
		return "", false
	}
	return v, true
}

// extra reads `[<prefix>N "value"]`, optionally followed by a space.
func (c *scanner) extra(prefix string) bool {
	if !c.consume("[" + prefix) {
		return false
	}
	end := strings.IndexByte(c.s, ' ')
	if end <= 0 {
		return false
	}
	for _, r := range c.s[:end] {
		if r < '0' || r > '9' {
			return false
		}
	}
	c.s = c.s[end+1:]
	if _, ok := c.quoted(); !ok || !c.consume("]") {
		return false
	}
	c.consume(" ")
	return true
}
