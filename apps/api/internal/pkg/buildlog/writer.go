package buildlog

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"

	"github.com/belune-hq/belune/internal/pkg/joblog"
	"github.com/belune-hq/belune/internal/pkg/loglevel"
)

// LogSink accumulates NDJSON build-log entries produced by one or more per-stream
// writers. For each complete line it infers a level, records the originating
// stream, publishes the line as an NDJSON entry to the Publisher (for live
// viewers), and accumulates the same entry for the stored build log. The
// per-stream writers let a build's stdout and stderr be tagged separately even
// though they interleave; Write may be called concurrently from both.
type LogSink struct {
	pub     *Publisher
	ctx     context.Context
	mu      sync.Mutex // guards log (concurrent stdout/stderr writers + NDJSON reader)
	log     joblog.Builder
	writers []*streamWriter
}

func NewLogSink(pub *Publisher, ctx context.Context) *LogSink {
	return &LogSink{pub: pub, ctx: ctx}
}

// Writer returns an io.Writer that tags every line it receives with `stream`
// (e.g. "stdout"/"stderr"). Create writers before the build starts; Flush drains
// all of them.
func (s *LogSink) Writer(stream string) io.Writer {
	w := &streamWriter{sink: s, stream: stream}
	s.writers = append(s.writers, w)
	return w
}

// Flush emits any buffered partial line from every registered writer. Call after
// the build finishes (its stream-copy goroutines have stopped).
func (s *LogSink) Flush() {
	for _, w := range s.writers {
		w.flush()
	}
}

// NDJSON returns the accumulated build log as newline-delimited JSON entries.
// Safe to call concurrently with the build's writes (e.g. from a periodic flusher).
func (s *LogSink) NDJSON() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.log.String()
}

func (s *LogSink) emit(stream, line string) {
	if line == "" {
		return
	}
	// The level is inferred from the message only. The stream is recorded but
	// deliberately not used for the level: build tools write most output
	// (including progress and info) to stderr, so a stderr=>Error fallback would
	// mislabel almost everything.
	e := joblog.Entry{
		Ts:     time.Now().UTC(),
		Level:  loglevel.Detect(line, ""),
		Stream: stream,
		Msg:    line,
	}
	s.mu.Lock()
	s.log.AddEntry(e)
	s.mu.Unlock()
	// Don't fail the build if a Redis publish fails.
	_ = s.pub.Publish(s.ctx, e.MarshalLine())
}

// streamWriter is one input stream into a LogSink. It buffers partial lines until
// a newline, then forwards each complete line to the sink tagged with its stream.
type streamWriter struct {
	sink   *LogSink
	stream string
	buf    bytes.Buffer
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadBytes('\n')
		if err != nil {
			// No complete line yet — put the partial data back.
			w.buf.Write(line)
			break
		}
		w.sink.emit(w.stream, string(bytes.TrimRight(line, "\n\r")))
	}
	return len(p), nil
}

func (w *streamWriter) flush() {
	if w.buf.Len() > 0 {
		remaining := w.buf.String()
		w.buf.Reset()
		w.sink.emit(w.stream, remaining)
	}
}
