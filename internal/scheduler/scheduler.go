// Package scheduler runs the loop that turns due jobs into queued runs.
//
// The scheduler decides WHEN a job should run; it never executes the job. Each
// tick it finds due jobs, enqueues one run per due occurrence (idempotently),
// and advances the job's next_run_at. Runs of jobs with event conditions are
// enqueued waiting; the tick then promotes waiting runs whose events have
// arrived and skips those whose wait deadline has passed.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"kronwrk/internal/db"
	"kronwrk/internal/models"
	"kronwrk/internal/schedule"
)

// Scheduler enqueues runs for due jobs on a fixed interval.
type Scheduler struct {
	store    *db.Store
	interval time.Duration
	id       string
	log      *slog.Logger
}

// New creates a Scheduler.
func New(store *db.Store, interval time.Duration, log *slog.Logger) *Scheduler {
	id := instanceID()
	return &Scheduler{store: store, interval: interval, id: id, log: log.With("scheduler_id", id)}
}

// instanceID uniquely identifies this scheduler process in the service_events log.
func instanceID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// Run ticks until ctx is cancelled, then returns. Cancellation is the graceful
// shutdown signal: the current tick finishes and the loop exits cleanly.
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info("scheduler started", "interval", s.interval)
	s.logEvent(models.EventStart)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		if err := s.tick(ctx); err != nil {
			s.log.Error("scheduler tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopped")
			s.logEvent(models.EventStop)
			return nil
		case <-ticker.C:
		}
	}
}

// logEvent records a lifecycle event in the service_events audit log. It uses a
// fresh, short context so the 'stop' event is written even though Run's ctx is
// already cancelled at shutdown; a failure is logged, never fatal.
func (s *Scheduler) logEvent(event string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.LogServiceEvent(ctx, models.ServiceScheduler, s.id, event); err != nil {
		s.log.Warn("record service event failed", "event", event, "err", err)
	}
}

func (s *Scheduler) tick(ctx context.Context) error {
	jobs, err := s.store.DueJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		// The due occurrence is the job's current next_run_at; that becomes the
		// run's deterministic scheduled_for.
		scheduledFor := *job.NextRunAt
		next, err := schedule.NextRun(job.ScheduleExpr, job.Timezone, scheduledFor)
		if err != nil {
			s.log.Error("compute next run failed", "job_id", job.ID, "err", err)
			continue
		}
		inserted, err := s.store.EnqueueRun(ctx, job.ID, job.Name, scheduledFor, next)
		if err != nil {
			s.log.Error("enqueue run failed", "job_id", job.ID, "err", err)
			continue
		}
		if inserted {
			s.log.Info("run queued", "job_id", job.ID, "scheduled_for", scheduledFor, "next_run_at", next)
		}
	}

	// Promotion before expiry: an event present at tick time beats a deadline
	// that lapsed within the same poll interval. Running in the same tick as
	// the enqueue loop also means an event emitted before the scheduled time
	// promotes the run with no extra latency.
	promoted, err := s.store.PromoteWaitingRuns(ctx)
	if err != nil {
		s.log.Error("promote waiting runs failed", "err", err)
	}
	for _, id := range promoted {
		s.log.Info("run promoted", "run_id", id)
	}
	skipped, err := s.store.ExpireWaitingRuns(ctx)
	if err != nil {
		s.log.Error("expire waiting runs failed", "err", err)
	} else if skipped > 0 {
		s.log.Info("waiting runs skipped past deadline", "count", skipped)
	}
	return nil
}
