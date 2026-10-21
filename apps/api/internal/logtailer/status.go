package logtailer

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Why this exists: request logging once sat dead for months behind a wrong
// ACCESS_LOG_PATH, and the only evidence was an empty table. An empty table is
// also what a quiet install looks like, so a bare "latest row" cannot tell the
// two apart. These counters can: a present file that has produced lines is not
// broken, one that produced none may only be idle, and one that is missing is
// the failure itself.

// File states, from stat'ing the configured path at the moment of the call.
const (
	FilePresent    = "present"
	FileMissing    = "missing"
	FileUnreadable = "unreadable"
)

// Ingestion states. Distinct on purpose: "no_traffic" and "no_app_traffic" are
// healthy-but-quiet, "missing"/"unreadable"/"error" are broken.
const (
	StateMissing      = "missing"        // the file does not exist
	StateUnreadable   = "unreadable"     // it exists but cannot be opened
	StateError        = "error"          // reading or inserting failed after the last success
	StateStarting     = "starting"       // file present, tailer not attached yet
	StateNoTraffic    = "no_traffic"     // attached; Caddy has logged nothing since
	StateNoAppTraffic = "no_app_traffic" // lines arrive, none for an application domain
	StateIngesting    = "ingesting"      // rows are being written
)

// counters is the tailer's running tally, since process start. The tailer seeks
// to the end of the file on attach, so lines logged before the API started are
// never counted: zero means "none since startedAt", not "none ever".
type counters struct {
	startedAt    time.Time
	tailing      bool
	linesRead    int64
	rowsInserted int64
	lastLineAt   time.Time
	lastInsertAt time.Time
	lastError    string
	lastErrorAt  time.Time
}

// Status is a snapshot of the tailer, for the dashboard.
type Status struct {
	Path string `json:"path"`
	// ResolvedPath is the absolute form of Path. The dev default is relative, and
	// what matters is where the process actually looks.
	ResolvedPath string `json:"resolved_path"`
	File         string `json:"file"`
	State        string `json:"state"`
	// Since is when counting began; everything below is relative to it.
	Since time.Time `json:"since"`
	// LinesRead counts non-empty lines of any host, RowsInserted those stored.
	// The gap between them is traffic for hosts that are not application domains
	// (the dashboard itself), which is normal.
	LinesRead    int64      `json:"lines_read"`
	RowsInserted int64      `json:"rows_inserted"`
	LastLineAt   *time.Time `json:"last_line_at,omitempty"`
	LastInsertAt *time.Time `json:"last_insert_at,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	LastErrorAt  *time.Time `json:"last_error_at,omitempty"`
}

// Status reports the tailer's health. The file is stat'ed here, not cached, so
// a path that appears or vanishes shows at once.
func (t *Tailer) Status() Status {
	file := inspectFile(t.logPath)

	t.statsMu.Lock()
	c := t.stats
	t.statsMu.Unlock()

	abs, err := filepath.Abs(t.logPath)
	if err != nil {
		abs = t.logPath
	}
	return Status{
		Path:         t.logPath,
		ResolvedPath: abs,
		File:         file,
		State:        classify(file, c),
		Since:        c.startedAt,
		LinesRead:    c.linesRead,
		RowsInserted: c.rowsInserted,
		LastLineAt:   optTime(c.lastLineAt),
		LastInsertAt: optTime(c.lastInsertAt),
		LastError:    c.lastError,
		LastErrorAt:  optTime(c.lastErrorAt),
	}
}

// classify is pure so every outcome can be tested without a file or a database.
func classify(file string, c counters) string {
	switch file {
	case FileMissing:
		return StateMissing
	case FileUnreadable:
		return StateUnreadable
	}
	// An error only counts while it is the latest word: a failed insert followed
	// by successful ones has recovered, and must not pin the panel red.
	if !c.lastErrorAt.IsZero() && c.lastErrorAt.After(c.lastInsertAt) {
		return StateError
	}
	switch {
	case !c.tailing:
		return StateStarting
	case c.rowsInserted > 0:
		return StateIngesting
	case c.linesRead > 0:
		return StateNoAppTraffic
	}
	return StateNoTraffic
}

func inspectFile(path string) string {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileMissing
		}
		return FileUnreadable
	}
	f, err := os.Open(path)
	if err != nil {
		return FileUnreadable
	}
	_ = f.Close()
	return FilePresent
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (t *Tailer) setTailing(v bool) {
	t.statsMu.Lock()
	t.stats.tailing = v
	t.statsMu.Unlock()
}

func (t *Tailer) countLine() {
	t.statsMu.Lock()
	t.stats.linesRead++
	t.stats.lastLineAt = time.Now()
	t.statsMu.Unlock()
}

func (t *Tailer) countInsert() {
	t.statsMu.Lock()
	t.stats.rowsInserted++
	t.stats.lastInsertAt = time.Now()
	t.statsMu.Unlock()
}

func (t *Tailer) recordError(err error) {
	t.statsMu.Lock()
	t.stats.lastError = err.Error()
	t.stats.lastErrorAt = time.Now()
	t.statsMu.Unlock()
}
