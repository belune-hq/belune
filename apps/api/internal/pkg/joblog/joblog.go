// Package joblog builds NDJSON log output for operation/run logs (deployments,
// database backups and restores). Each log line is a JSON object
//
//	{"ts":"2006-01-02T15:04:05.000Z","level":"info","msg":"…"}
//
// stored one-per-line in the run's TEXT column. Storing the timestamp and level
// as real fields (instead of embedding them in the message text) lets the log
// viewer localize the time and align/filter by level exactly like container
// logs. The format mirrors the frontend parser in
// apps/web/src/components/logs/parse.ts.
package joblog

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/belune-hq/belune/internal/pkg/loglevel"
)

// Entry is a single log line. Stream records the originating OS stream
// ("stdout"/"stderr") of verbatim tool/build output when known; it is captured
// for completeness (not currently surfaced in the viewer, which shows level
// only) and is empty for our own app-generated messages.
type Entry struct {
	Ts     time.Time      `json:"ts"`
	Level  loglevel.Level `json:"level"`
	Stream string         `json:"stream,omitempty"`
	Msg    string         `json:"msg"`
}

// MarshalLine renders one entry as a single NDJSON line (no trailing newline).
func (e Entry) MarshalLine() string {
	b, err := json.Marshal(e)
	if err != nil {
		// Fall back to a minimal valid object so a bad line never corrupts the
		// stream or loses the message.
		b, _ = json.Marshal(Entry{Ts: e.Ts, Level: loglevel.Info, Msg: e.Msg})
	}
	return string(b)
}

// Builder accumulates entries and renders them as NDJSON. It is not safe for
// concurrent use; callers serialize writes (the backup runLog is single-
// goroutine; the build LogSink guards it with a mutex).
type Builder struct {
	sb strings.Builder
}

// Add appends an entry with an explicit level, stamped at the current time.
func (b *Builder) Add(level loglevel.Level, msg string) {
	b.add(Entry{Ts: time.Now().UTC(), Level: level, Msg: msg})
}

// AddDetected appends an entry whose level is inferred from the message and the
// originating stream (used for verbatim tool/build output). The stream is also
// recorded on the entry.
func (b *Builder) AddDetected(stream, msg string) {
	b.add(Entry{Ts: time.Now().UTC(), Level: loglevel.Detect(msg, stream), Stream: stream, Msg: msg})
}

// AddRaw splits verbatim multi-line output into one detected-level entry per
// non-empty line. Handy for captured command stderr/stdout.
func (b *Builder) AddRaw(stream, text string) {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if line != "" {
			b.AddDetected(stream, line)
		}
	}
}

// AddEntry appends a pre-built entry.
func (b *Builder) AddEntry(e Entry) { b.add(e) }

func (b *Builder) add(e Entry) {
	b.sb.WriteString(e.MarshalLine())
	b.sb.WriteByte('\n')
}

// String returns the accumulated NDJSON.
func (b *Builder) String() string { return b.sb.String() }

// Len reports how many bytes have been accumulated.
func (b *Builder) Len() int { return b.sb.Len() }
