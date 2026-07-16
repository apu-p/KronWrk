// Package models holds the domain types shared across the scheduler, worker,
// and CLI.
package models

import "time"

// Run status values. A run is created queued (or waiting, when its job has
// event conditions), claimed into running, and ends in one terminal state.
// waiting → queued when every condition has a matching event; waiting →
// skipped (terminal) when the wait deadline passes first.
const (
	StatusWaiting   = "waiting"
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusTimedOut  = "timed_out"
	StatusSkipped   = "skipped"
)

// Long-lived service names and lifecycle events recorded in service_events.
// start/stop are written by the daemon itself over its own connection, so
// their username/role columns show the identity the daemon runs as. The
// *_requested events are written by the shell's `daemon start|stop` over the
// operator's session connection, recording who asked — a SIGTERM carries no
// sender identity, so this is the only place the actor can be captured.
const (
	ServiceScheduler = "scheduler"
	ServiceWorker    = "worker"

	EventStart          = "start"
	EventStop           = "stop"
	EventStartRequested = "start_requested"
	EventStopRequested  = "stop_requested"
)

// DBUser is a Postgres login role managed by `kronwrk user`. Roles holds the
// kronwrk_* group memberships — exactly one when healthy; a slice so that a
// drifted multi-role state can be revealed rather than hidden. CreateRole
// marks the operator login (user management needs the CREATEROLE attribute,
// which is not inherited through group membership).
type DBUser struct {
	Username   string
	Roles      []string
	Superuser  bool
	CreateRole bool
}

// Job is a scheduled job definition (the jobs table).
type Job struct {
	ID                int64
	Name              string
	Command           string
	Args              []string
	ScheduleExpr      string
	Timezone          string
	NextRunAt         *time.Time
	Enabled           bool
	AllowOverlap      bool
	MaxConcurrentRuns int
	MisfirePolicy     string
	TimeoutSeconds    int
	MaxRetries        int
	Comment           string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// JobOverview is one row of the monitor view: a job joined with its most
// recent run, if it has ever had one. LastRunAt is the run's start time
// (falling back to scheduled_for while it is still queued); LastStatus is nil
// when the job has never run.
type JobOverview struct {
	ID           int64
	Name         string
	ScheduleExpr string
	Timezone     string
	Enabled      bool
	NextRunAt    *time.Time
	LastRunAt    *time.Time
	LastStatus   *string
}

// JobRun is one execution attempt (the job_runs table). WaitDeadline is set
// only on runs of conditioned jobs: when the deadline passes while the run is
// still waiting it becomes skipped; nil means no deadline (wait forever, or an
// unconditioned job).
type JobRun struct {
	ID              int64
	JobID           int64
	JobName         string
	ScheduledFor    time.Time
	Status          string
	WorkerID        *string
	Attempt         int
	StartedAt       *time.Time
	FinishedAt      *time.Time
	LastHeartbeatAt *time.Time
	LeaseExpiresAt  *time.Time
	ExitCode        *int
	ErrorMessage    *string
	WaitDeadline    *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// JobCondition gates a job's runs on an emitted event (the job_conditions
// table). WaitSeconds bounds how long past scheduled_for a run waits for the
// event; 0 means wait forever.
type JobCondition struct {
	JobID       int64
	EventName   string
	WaitSeconds int
	CreatedAt   time.Time
}

// Event is one emitted fact (the events table). ConsumedByRunID is set once
// the event has satisfied a waiting run — consume-on-match, so each event
// gates at most one run. Payload is raw JSON, nil when absent.
type Event struct {
	ID              int64
	Name            string
	Payload         []byte
	EmittedBy       string
	EmittedAt       time.Time
	ConsumedByRunID *int64
}
