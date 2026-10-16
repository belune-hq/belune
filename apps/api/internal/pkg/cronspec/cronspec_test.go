package cronspec

import (
	"testing"
	"time"
)

func TestParseHonoursTimezone(t *testing.T) {
	sched, err := Parse("0 3 * * *", "Asia/Kuala_Lumpur")
	if err != nil {
		t.Fatal(err)
	}
	// 03:00 MYT (UTC+8) is 19:00 UTC the previous day.
	got := sched.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).UTC()
	want := time.Date(2026, 1, 1, 19, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestParseEmptyTimezoneIsUTC(t *testing.T) {
	sched, err := Parse("0 3 * * *", "")
	if err != nil {
		t.Fatal(err)
	}
	got := sched.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).UTC()
	if want := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next = %v, want %v", got, want)
	}
}

func TestParseRejects(t *testing.T) {
	for name, c := range map[string][2]string{
		"bad zone":         {"0 3 * * *", "Mars/Olympus"},
		"embedded CRON_TZ": {"CRON_TZ=UTC 0 3 * * *", "UTC"},
		"embedded TZ":      {"TZ=UTC 0 3 * * *", "UTC"},
		"six fields":       {"* * * * * *", "UTC"},
		"garbage":          {"not a cron", "UTC"},
	} {
		if _, err := Parse(c[0], c[1]); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseMinimumMinute(t *testing.T) {
	for _, ok := range []string{"* * * * *", "@every 1m", "@every 90s", "@hourly"} {
		if _, err := ParseMinimumMinute(ok, "UTC"); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	if _, err := ParseMinimumMinute("@every 30s", "UTC"); err == nil {
		t.Error("@every 30s should be rejected")
	}
}
