// Package schedule evaluates cron expressions to compute run times.
//
// Cron parsing is delegated to a maintained library (robfig/cron); we never
// parse expressions by hand. Evaluation happens in the job's own timezone.
package schedule

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// NextRun returns the next occurrence of a standard 5-field cron expression
// strictly after `after`, evaluated in the given IANA timezone (e.g. "UTC",
// "America/New_York"). A zero time is returned only with a non-nil error.
func NextRun(expr, timezone string, after time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("load timezone %q: %w", timezone, err)
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse schedule %q: %w", expr, err)
	}
	return sched.Next(after.In(loc)), nil
}
