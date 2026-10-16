// Package cronspec parses a stored cron expression together with its own
// timezone. robfig/cron's ParseStandard resolves a bare expression in
// time.Local, which is only UTC by accident of the image's /etc/localtime, so
// every schedule that carries a timezone column goes through here instead.
package cronspec

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// DefaultTimezone is what a schedule without an explicit zone means.
const DefaultTimezone = "UTC"

// ValidateTimezone rejects a zone time.LoadLocation cannot resolve. It is called
// at write time: a typo'd zone must be a 400, not a schedule that never fires.
func ValidateTimezone(tz string) error {
	if tz == "" {
		return errors.New("timezone is required")
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("unknown timezone %q", tz)
	}
	return nil
}

// Parse returns the schedule for expr evaluated in tz. An empty tz means UTC.
// The stored expr stays a clean 5-field cron; the zone is composed in here.
func Parse(expr, tz string) (cron.Schedule, error) {
	if tz == "" {
		tz = DefaultTimezone
	}
	if err := ValidateTimezone(tz); err != nil {
		return nil, err
	}
	// A zone embedded in the expression would be a second, conflicting one.
	if up := strings.ToUpper(strings.TrimSpace(expr)); strings.HasPrefix(up, "TZ=") || strings.HasPrefix(up, "CRON_TZ=") {
		return nil, errors.New("set the timezone in its own field, not in the cron expression")
	}
	return cron.ParseStandard("CRON_TZ=" + tz + " " + expr)
}

// ParseMinimumMinute is Parse for callers driven by the per-minute sweep: it
// also rejects a schedule that can fire more often than once a minute, which
// would silently under-fire (`@every 30s` parses fine).
func ParseMinimumMinute(expr, tz string) (cron.Schedule, error) {
	sched, err := Parse(expr, tz)
	if err != nil {
		return nil, err
	}
	if c, ok := sched.(cron.ConstantDelaySchedule); ok && c.Delay < time.Minute {
		return nil, errors.New("schedules must be at least one minute apart")
	}
	return sched, nil
}
