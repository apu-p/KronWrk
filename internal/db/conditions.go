package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"kronwrk/internal/models"
)

// Event gating (migration 0014): jobs with rows in job_conditions have their
// runs enqueued as waiting; each scheduler tick promotes waiting runs whose
// every condition has an unconsumed matching event, and skips those whose
// wait deadline has passed. Jobs without conditions are untouched.

const eventColumns = `id, name, payload, emitted_by, emitted_at, consumed_by_run_id`

func scanEvent(s scanner) (models.Event, error) {
	var e models.Event
	err := s.Scan(&e.ID, &e.Name, &e.Payload, &e.EmittedBy, &e.EmittedAt, &e.ConsumedByRunID)
	return e, err
}

// waitPlan decides a new run's initial status and wait deadline from the wait
// windows of its job's conditions. No conditions: queued immediately, no
// deadline. Any wait_seconds of 0 (wait forever) dominates: waiting with no
// deadline. Otherwise: waiting until scheduled_for + max(wait_seconds).
func waitPlan(scheduledFor time.Time, waitSeconds []int) (status string, deadline *time.Time) {
	if len(waitSeconds) == 0 {
		return models.StatusQueued, nil
	}
	max := 0
	for _, w := range waitSeconds {
		if w == 0 {
			return models.StatusWaiting, nil
		}
		if w > max {
			max = w
		}
	}
	d := scheduledFor.Add(time.Duration(max) * time.Second)
	return models.StatusWaiting, &d
}

// conditionWaits loads the wait windows of a job's conditions inside tx.
func conditionWaits(ctx context.Context, tx pgx.Tx, jobID int64) ([]int, error) {
	rows, err := tx.Query(ctx, `SELECT wait_seconds FROM job_conditions WHERE job_id = $1`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var waits []int
	for rows.Next() {
		var w int
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		waits = append(waits, w)
	}
	return waits, rows.Err()
}

// PromoteWaitingRuns promotes waiting runs whose every condition has an
// unconsumed matching event, consuming those events (oldest first; each event
// satisfies at most one run). One transaction per run with the run and event
// rows locked FOR UPDATE SKIP LOCKED — the same discipline as ClaimRun — so
// concurrent schedulers never double-consume an event or double-promote a
// run. Returns the ids of the promoted runs.
func (s *Store) PromoteWaitingRuns(ctx context.Context) ([]int64, error) {
	// Stale-snapshot candidate list; each candidate is re-checked under lock.
	// Oldest scheduled first so earlier occurrences consume events first.
	rows, err := s.pool.Query(ctx, `
		SELECT id, job_id FROM job_runs
		WHERE status = $1
		ORDER BY scheduled_for, id`, models.StatusWaiting)
	if err != nil {
		return nil, err
	}
	type candidate struct{ runID, jobID int64 }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.runID, &c.jobID); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var promoted []int64
	for _, c := range candidates {
		ok, err := s.promoteRun(ctx, c.runID, c.jobID)
		if err != nil {
			return promoted, fmt.Errorf("promote run %d: %w", c.runID, err)
		}
		if ok {
			promoted = append(promoted, c.runID)
		}
	}
	return promoted, nil
}

// promoteRun attempts to promote one waiting run, consuming one unconsumed
// event per condition atomically. Returns false (no error) when the run is no
// longer waiting, another scheduler holds it, or some condition has no event
// yet — the run simply stays waiting until a later tick.
func (s *Store) promoteRun(ctx context.Context, runID, jobID int64) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback is a no-op after commit

	// Re-check and lock the run; SKIP LOCKED yields to a concurrent scheduler.
	var id int64
	err = tx.QueryRow(ctx, `
		SELECT id FROM job_runs
		WHERE id = $1 AND status = $2
		FOR UPDATE SKIP LOCKED`, runID, models.StatusWaiting).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	names, err := conditionNames(ctx, tx, jobID)
	if err != nil {
		return false, err
	}
	// Zero conditions (all removed while the run waited) is vacuously
	// satisfied: removing a job's conditions un-gates its pending runs.

	// One unconsumed event per condition, oldest first. UNIQUE(job_id,
	// event_name) guarantees distinct names, so no event is matched twice.
	eventIDs := make([]int64, 0, len(names))
	for _, name := range names {
		var eventID int64
		err := tx.QueryRow(ctx, `
			SELECT id FROM events
			WHERE name = $1 AND consumed_by_run_id IS NULL
			ORDER BY emitted_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1`, name).Scan(&eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil // condition unmet; rollback releases the locks
		}
		if err != nil {
			return false, err
		}
		eventIDs = append(eventIDs, eventID)
	}

	if len(eventIDs) > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE events SET consumed_by_run_id = $1 WHERE id = ANY($2)`,
			runID, eventIDs); err != nil {
			return false, fmt.Errorf("consume events: %w", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE job_runs SET status = $1, updated_at = now() WHERE id = $2`,
		models.StatusQueued, runID); err != nil {
		return false, fmt.Errorf("promote run: %w", err)
	}
	return true, tx.Commit(ctx)
}

// conditionNames loads the event names a job's runs wait for, inside tx.
func conditionNames(ctx context.Context, tx pgx.Tx, jobID int64) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT event_name FROM job_conditions WHERE job_id = $1`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// ExpireWaitingRuns marks waiting runs whose deadline has passed as skipped.
// Skipped is terminal: finished_at is set so run history orders these with
// finished runs, not in-flight ones. Returns the number of runs skipped.
func (s *Store) ExpireWaitingRuns(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE job_runs
		SET status = $1, finished_at = now(), updated_at = now()
		WHERE status = $2 AND wait_deadline IS NOT NULL AND wait_deadline <= now()`,
		models.StatusSkipped, models.StatusWaiting)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// EmitEvent records an emitted event. payload is raw JSON (nil for none);
// emitted_by defaults server-side to the connected login.
func (s *Store) EmitEvent(ctx context.Context, name string, payload []byte) (models.Event, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO events (name, payload)
		VALUES ($1, $2)
		RETURNING `+eventColumns, name, payload)
	return scanEvent(row)
}

// ListEvents returns the newest events first, capped at limit.
func (s *Store) ListEvents(ctx context.Context, limit int) ([]models.Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+eventColumns+` FROM events
		ORDER BY emitted_at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []models.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// AddJobCondition gates a job's future runs on eventName. waitSeconds bounds
// how long past its scheduled time a run waits (0 = forever). Takes effect on
// runs enqueued after the call; already-queued runs are not re-gated.
func (s *Store) AddJobCondition(ctx context.Context, jobID int64, eventName string, waitSeconds int) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO job_conditions (job_id, event_name, wait_seconds)
		VALUES ($1, $2, $3)`, jobID, eventName, waitSeconds)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case "23505": // unique_violation
				return fmt.Errorf("job %d already has a condition on event %q", jobID, eventName)
			case "23503": // foreign_key_violation
				return fmt.Errorf("no job with id %d", jobID)
			}
		}
		return err
	}
	return nil
}

// ListJobConditions returns a job's conditions ordered by event name.
func (s *Store) ListJobConditions(ctx context.Context, jobID int64) ([]models.JobCondition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT job_id, event_name, wait_seconds, created_at
		FROM job_conditions
		WHERE job_id = $1
		ORDER BY event_name`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conds []models.JobCondition
	for rows.Next() {
		var c models.JobCondition
		if err := rows.Scan(&c.JobID, &c.EventName, &c.WaitSeconds, &c.CreatedAt); err != nil {
			return nil, err
		}
		conds = append(conds, c)
	}
	return conds, rows.Err()
}

// RemoveJobCondition drops one condition. A run already waiting on it is
// un-gated on the next tick if this was the job's last unmet condition.
func (s *Store) RemoveJobCondition(ctx context.Context, jobID int64, eventName string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM job_conditions WHERE job_id = $1 AND event_name = $2`, jobID, eventName)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("job %d has no condition on event %q", jobID, eventName)
	}
	return nil
}
