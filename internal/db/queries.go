package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"kronwrk/internal/models"
	"kronwrk/internal/schedule"
)

// ErrNoRun is returned by ClaimRun when no queued run is available.
var ErrNoRun = errors.New("no queued run available")

// Store is the data-access layer over the pgx pool. All SQL lives here in a
// thin, reviewable layer (pgx with hand-written SQL, no ORM).
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// scanner is satisfied by both pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

const jobColumns = `id, name, command, args, schedule_expr, timezone, next_run_at,
	enabled, allow_overlap, max_concurrent_runs, misfire_policy, timeout_seconds,
	max_retries, comment, created_at, updated_at`

func scanJob(s scanner) (models.Job, error) {
	var j models.Job
	err := s.Scan(&j.ID, &j.Name, &j.Command, &j.Args, &j.ScheduleExpr, &j.Timezone,
		&j.NextRunAt, &j.Enabled, &j.AllowOverlap, &j.MaxConcurrentRuns, &j.MisfirePolicy,
		&j.TimeoutSeconds, &j.MaxRetries, &j.Comment, &j.CreatedAt, &j.UpdatedAt)
	return j, err
}

const runColumns = `id, job_id, job_name, scheduled_for, status, worker_id, attempt,
	started_at, finished_at, last_heartbeat_at, lease_expires_at, exit_code,
	error_message, wait_deadline, created_at, updated_at`

func scanRun(s scanner) (models.JobRun, error) {
	var r models.JobRun
	err := s.Scan(&r.ID, &r.JobID, &r.JobName, &r.ScheduledFor, &r.Status, &r.WorkerID, &r.Attempt,
		&r.StartedAt, &r.FinishedAt, &r.LastHeartbeatAt, &r.LeaseExpiresAt, &r.ExitCode,
		&r.ErrorMessage, &r.WaitDeadline, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// InsertJob creates a job definition, computing its initial next_run_at from the
// schedule expression. The stored job (with id and timestamps) is returned.
func (s *Store) InsertJob(ctx context.Context, j models.Job) (models.Job, error) {
	next, err := schedule.NextRun(j.ScheduleExpr, j.Timezone, time.Now())
	if err != nil {
		return models.Job{}, err
	}
	// A nil slice encodes as SQL NULL, which violates the NOT NULL args column;
	// normalize to an empty array so a job with no arguments is valid.
	if j.Args == nil {
		j.Args = []string{}
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO jobs (name, command, args, schedule_expr, timezone, next_run_at, timeout_seconds, max_retries, comment)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING `+jobColumns,
		j.Name, j.Command, j.Args, j.ScheduleExpr, j.Timezone, next, j.TimeoutSeconds, j.MaxRetries, j.Comment)
	job, err := scanJob(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return models.Job{}, fmt.Errorf("a job with name %q, command %q, and schedule %q already exists",
				j.Name, j.Command, j.ScheduleExpr)
		}
		return models.Job{}, err
	}
	return job, nil
}

// ListJobs returns all jobs ordered by id.
func (s *Store) ListJobs(ctx context.Context) ([]models.Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+jobColumns+` FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []models.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// GetJob fetches a single job by id.
func (s *Store) GetJob(ctx context.Context, id int64) (models.Job, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id)
	return scanJob(row)
}

// setJobEnabled flips a job's enabled flag. It touches only enabled/updated_at
// so the support role's column-level UPDATE grant covers it; in particular it
// never recomputes next_run_at — the scheduler catches up from the stored
// value on its next tick.
func (s *Store) setJobEnabled(ctx context.Context, id int64, enabled bool) error {
	tag, err := s.pool.Exec(ctx, `UPDATE jobs SET enabled = $1, updated_at = now() WHERE id = $2`, enabled, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("no job with id %d", id)
	}
	return nil
}

// DisableJob marks a job inactive so the scheduler stops running it.
func (s *Store) DisableJob(ctx context.Context, id int64) error {
	return s.setJobEnabled(ctx, id, false)
}

// EnableJob re-activates a job so the scheduler resumes running it.
func (s *Store) EnableJob(ctx context.Context, id int64) error {
	return s.setJobEnabled(ctx, id, true)
}

// PreflightScheduler verifies the connection holds the privileges the
// scheduler loop needs (read/lock jobs, insert runs) without touching any
// rows. Postgres checks table ACLs even when WHERE false matches nothing, so
// a role lacking access fails here — at startup — rather than error-logging on
// every tick.
func (s *Store) PreflightScheduler(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `SELECT id FROM jobs WHERE false FOR UPDATE`); err != nil {
		return err
	}
	// Mirror EnqueueRun's ON CONFLICT clause: conflict detection needs SELECT
	// on the arbiter-index columns on top of INSERT, so a plain test INSERT
	// would pass preflight and still fail on every real enqueue.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO job_runs (job_id, job_name, scheduled_for, status, wait_deadline)
		SELECT 0, '', now(), '', NULL WHERE false
		ON CONFLICT (job_id, scheduled_for) DO NOTHING`); err != nil {
		return err
	}
	// Event gating (0014): the promotion/expiry passes read conditions, lock
	// and consume events, and lock/flip waiting runs.
	probes := []string{
		`SELECT wait_seconds FROM job_conditions WHERE false`,
		`SELECT id FROM events WHERE false FOR UPDATE`,
		`UPDATE events SET consumed_by_run_id = 0 WHERE false`,
		`SELECT id FROM job_runs WHERE false FOR UPDATE`,
		`UPDATE job_runs SET status = '', finished_at = now(), updated_at = now() WHERE false`,
	}
	for _, q := range probes {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// PreflightWorker verifies the connection can claim and update runs, without
// touching any rows (see PreflightScheduler).
func (s *Store) PreflightWorker(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `SELECT id FROM job_runs WHERE false FOR UPDATE`)
	return err
}

// ListJobOverviews returns every job joined with its latest run (newest
// scheduled_for wins, so a queued attempt counts as the latest), for the
// monitor view. Requires SELECT on both jobs and job_runs.
func (s *Store) ListJobOverviews(ctx context.Context) ([]models.JobOverview, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT j.id, j.name, j.schedule_expr, j.timezone, j.enabled, j.next_run_at,
		       COALESCE(r.started_at, r.scheduled_for), r.status
		FROM jobs j
		LEFT JOIN LATERAL (
			SELECT status, started_at, scheduled_for
			FROM job_runs
			WHERE job_id = j.id
			ORDER BY scheduled_for DESC
			LIMIT 1
		) r ON true
		ORDER BY j.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.JobOverview
	for rows.Next() {
		var o models.JobOverview
		if err := rows.Scan(&o.ID, &o.Name, &o.ScheduleExpr, &o.Timezone, &o.Enabled,
			&o.NextRunAt, &o.LastRunAt, &o.LastStatus); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListRunsForJob returns one job's run history for the monitor drill-down:
// in-flight attempts first (queued/running rows have finished_at IS NULL —
// only FinalizeRun sets it), then finished runs newest-first. Capped so a
// job with years of history stays snappy in the TUI.
func (s *Store) ListRunsForJob(ctx context.Context, jobID int64) ([]models.JobRun, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+runColumns+` FROM job_runs
		WHERE job_id = $1
		ORDER BY finished_at DESC NULLS FIRST, scheduled_for DESC
		LIMIT 200`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []models.JobRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// GetRun fetches a single run by id.
func (s *Store) GetRun(ctx context.Context, id int64) (models.JobRun, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM job_runs WHERE id = $1`, id)
	return scanRun(row)
}

// DueJobs returns enabled jobs whose next_run_at is at or before now.
func (s *Store) DueJobs(ctx context.Context) ([]models.Job, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE enabled AND next_run_at IS NOT NULL AND next_run_at <= now()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []models.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// EnqueueRun inserts a run for (jobID, scheduledFor) and advances the job's
// next_run_at, atomically in one transaction. The insert is idempotent via
// ON CONFLICT DO NOTHING on UNIQUE (job_id, scheduled_for), so a scheduler
// restart cannot create duplicate runs. A job with event conditions gets a
// waiting run (promoted later by PromoteWaitingRuns); otherwise the run is
// queued immediately. Returns true if a new run was inserted.
func (s *Store) EnqueueRun(ctx context.Context, jobID int64, jobName string, scheduledFor, nextRun time.Time) (inserted bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback is a no-op after commit

	waits, err := conditionWaits(ctx, tx, jobID)
	if err != nil {
		return false, fmt.Errorf("load conditions: %w", err)
	}
	status, deadline := waitPlan(scheduledFor, waits)

	tag, err := tx.Exec(ctx, `
		INSERT INTO job_runs (job_id, job_name, scheduled_for, status, wait_deadline)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (job_id, scheduled_for) DO NOTHING`,
		jobID, jobName, scheduledFor, status, deadline)
	if err != nil {
		return false, fmt.Errorf("insert run: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET next_run_at = $1, updated_at = now() WHERE id = $2`,
		nextRun, jobID); err != nil {
		return false, fmt.Errorf("advance next_run_at: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ClaimRun atomically claims one queued run for workerID, marking it running
// with a lease. Uses FOR UPDATE SKIP LOCKED so concurrent workers never claim
// the same run. Returns ErrNoRun when nothing is queued.
func (s *Store) ClaimRun(ctx context.Context, workerID string, lease time.Duration) (models.JobRun, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return models.JobRun{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var runID int64
	err = tx.QueryRow(ctx, `
		SELECT id FROM job_runs
		WHERE status = $1
		ORDER BY scheduled_for
		FOR UPDATE SKIP LOCKED
		LIMIT 1`, models.StatusQueued).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.JobRun{}, ErrNoRun
	}
	if err != nil {
		return models.JobRun{}, err
	}

	row := tx.QueryRow(ctx, `
		UPDATE job_runs
		SET status = $1, worker_id = $2, started_at = now(),
		    last_heartbeat_at = now(), lease_expires_at = now() + $3::interval,
		    updated_at = now()
		WHERE id = $4
		RETURNING `+runColumns,
		models.StatusRunning, workerID, lease.String(), runID)
	run, err := scanRun(row)
	if err != nil {
		return models.JobRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return models.JobRun{}, err
	}
	return run, nil
}

// Heartbeat renews a running run's lease.
func (s *Store) Heartbeat(ctx context.Context, runID int64, lease time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE job_runs
		SET last_heartbeat_at = now(), lease_expires_at = now() + $1::interval, updated_at = now()
		WHERE id = $2`,
		lease.String(), runID)
	return err
}

// FinalizeRun records a run's terminal status and result.
func (s *Store) FinalizeRun(ctx context.Context, runID int64, status string, exitCode *int, errMsg *string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE job_runs
		SET status = $1, finished_at = now(), exit_code = $2, error_message = $3, updated_at = now()
		WHERE id = $4`,
		status, exitCode, errMsg, runID)
	return err
}

// LogServiceEvent appends a lifecycle event ('start'/'stop') for a long-lived
// service ('scheduler'/'worker') to the service_events audit log, stamping the
// connected login and its kronwrk group role server-side — current_user and a
// direct-membership lookup (same shape as WhoAmI's), so the audit row reflects
// who actually connected, not anything the process claims. role_name is NULL
// for logins outside the role model (e.g. a superuser-run daemon).
func (s *Store) LogServiceEvent(ctx context.Context, service, instanceID, event string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO service_events (service, instance_id, event, username, role_name)
		VALUES ($1, $2, $3, current_user,
			(SELECT string_agg(g.rolname, ',' ORDER BY g.rolname)
			 FROM pg_auth_members am
			 JOIN pg_roles m ON m.oid = am.member
			 JOIN pg_roles g ON g.oid = am.roleid
			 WHERE m.rolname = current_user AND am.inherit_option
			   AND g.rolname = ANY($4)))`,
		service, instanceID, event, groupRoleValues())
	return err
}
