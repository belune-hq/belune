package logtailer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The three outcomes this exists to separate. A bare "latest row" query reports
// all of them as "nothing", which is how a dead path went unnoticed once.
func TestClassifySeparatesQuietFromBroken(t *testing.T) {
	now := time.Now()
	attached := counters{tailing: true, startedAt: now}

	tests := []struct {
		name string
		file string
		c    counters
		want string
	}{
		{"file missing is broken, whatever the counters say", FileMissing, attached, StateMissing},
		{"file unreadable is broken", FileUnreadable, attached, StateUnreadable},
		{"present and attached with no lines is merely quiet", FilePresent, attached, StateNoTraffic},
		{"lines for other hosts only", FilePresent, counters{tailing: true, linesRead: 4}, StateNoAppTraffic},
		{"rows stored", FilePresent, counters{tailing: true, linesRead: 4, rowsInserted: 2, lastInsertAt: now}, StateIngesting},
		{"not attached yet", FilePresent, counters{}, StateStarting},
		{"error after the last success", FilePresent,
			counters{tailing: true, rowsInserted: 2, lastInsertAt: now.Add(-time.Minute), lastErrorAt: now}, StateError},
		{"error that was since recovered from", FilePresent,
			counters{tailing: true, rowsInserted: 3, lastErrorAt: now.Add(-time.Minute), lastInsertAt: now}, StateIngesting},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.file, tt.c); got != tt.want {
				t.Errorf("classify = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInspectFile(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "access.log")
	if err := os.WriteFile(present, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := inspectFile(present); got != FilePresent {
		t.Errorf("present file = %q", got)
	}
	if got := inspectFile(filepath.Join(dir, "nope.log")); got != FileMissing {
		t.Errorf("missing file = %q", got)
	}
}

func TestStatusReportsResolvedPathAndCounters(t *testing.T) {
	tl := New("rel/access.log", nil, nil)
	tl.setTailing(true)
	tl.countLine()
	tl.countLine()
	tl.countInsert()

	st := tl.Status()
	if st.Path != "rel/access.log" || !filepath.IsAbs(st.ResolvedPath) {
		t.Errorf("path = %q resolved = %q", st.Path, st.ResolvedPath)
	}
	if st.LinesRead != 2 || st.RowsInserted != 1 || st.LastInsertAt == nil {
		t.Errorf("counters = %+v", st)
	}
	// The file does not exist, so the state must be missing even though the
	// tailer believes it is attached: the stat is live, not remembered.
	if st.State != StateMissing {
		t.Errorf("state = %q, want %q", st.State, StateMissing)
	}
}
