// Package worker claims queued runs and executes their commands.
//
// The worker owns execution and status updates while a run is active. It runs a
// fixed-size pool, claims runs with FOR UPDATE SKIP LOCKED, executes each
// command in its own process group with a timeout and bounded output capture,
// heartbeats the lease while running, and records a terminal status.
package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"kronwrk/internal/db"
	"kronwrk/internal/models"
)

// maxOutputBytes caps how much stderr we retain per run so a chatty job
// cannot exhaust memory.
const maxOutputBytes = 1 << 20 // 1 MiB

// Worker is a pool of execution loops sharing one store.
type Worker struct {
	store          *db.Store
	id             string
	concurrency    int
	pollInterval   time.Duration
	heartbeat      time.Duration
	defaultTimeout time.Duration
	log            *slog.Logger
}

// New creates a Worker. id should uniquely identify this process.
func New(store *db.Store, concurrency int, pollInterval, heartbeat, defaultTimeout time.Duration, log *slog.Logger) *Worker {
	id := workerID()
	return &Worker{
		store:          store,
		id:             id,
		concurrency:    concurrency,
		pollInterval:   pollInterval,
		heartbeat:      heartbeat,
		defaultTimeout: defaultTimeout,
		log:            log.With("worker_id", id),
	}
}

func workerID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// lease is how long a claim/heartbeat is valid before the run is considered
// orphaned. Generous relative to the heartbeat interval to tolerate jitter.
func (w *Worker) lease() time.Duration { return w.heartbeat * 3 }

// Run starts the pool and blocks until ctx is cancelled. On cancellation each
// loop stops claiming new work; in-flight runs are allowed to finish.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("worker started", "concurrency", w.concurrency)
	w.logEvent(models.EventStart)
	var wg sync.WaitGroup
	for i := 0; i < w.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	wg.Wait()
	w.log.Info("worker stopped")
	w.logEvent(models.EventStop)
	return nil
}

// logEvent records a lifecycle event in the service_events audit log. It uses a
// fresh, short context so the 'stop' event is written even though Run's ctx is
// already cancelled at shutdown; a failure is logged, never fatal.
func (w *Worker) logEvent(event string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.store.LogServiceEvent(ctx, models.ServiceWorker, w.id, event); err != nil {
		w.log.Warn("record service event failed", "event", event, "err", err)
	}
}

func (w *Worker) loop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		run, err := w.store.ClaimRun(ctx, w.id, w.lease())
		if errors.Is(err, db.ErrNoRun) {
			if !sleep(ctx, w.pollInterval) {
				return
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.log.Error("claim failed", "err", err)
			if !sleep(ctx, w.pollInterval) {
				return
			}
			continue
		}

		job, err := w.store.GetJob(ctx, run.JobID)
		if err != nil {
			w.log.Error("fetch job failed", "run_id", run.ID, "err", err)
			w.finalize(run.ID, models.StatusFailed, nil, ptr("fetch job: "+err.Error()))
			continue
		}
		w.execute(run, job)
	}
}

// execute runs the command for a claimed run and records the outcome. The
// execution context is derived from Background (not the shutdown context) so an
// in-flight run finishes on graceful shutdown, bounded by its own timeout.
func (w *Worker) execute(run models.JobRun, job models.Job) {
	timeout := w.defaultTimeout
	if job.TimeoutSeconds > 0 {
		timeout = time.Duration(job.TimeoutSeconds) * time.Second
	}
	execCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, job.Command, job.Args...)
	// Job commands run with a curated environment, never the worker's own: the
	// worker process holds the daemon's DATABASE_URL (with its password) and the
	// other kronwrk_* config, and a nil cmd.Env would hand all of it to every
	// job — letting any job exfiltrate the daemon's database credential. Pass
	// through only a safe allowlist (see jobEnv).
	cmd.Env = jobEnv()
	// Own process group so a timeout kills child processes too, not just the parent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// stdout is discarded (cmd.Stdout nil) until the run_logs table exists;
	// stderr feeds the error_message on failure.
	var stderr boundedBuffer
	cmd.Stderr = &stderr

	// Heartbeat the lease while the command runs.
	hbCtx, stopHeartbeat := context.WithCancel(context.Background())
	go w.heartbeatLoop(hbCtx, run.ID)

	w.log.Info("run started", "run_id", run.ID, "job_id", job.ID, "command", job.Command)
	runErr := cmd.Run()
	stopHeartbeat()

	status, exitCode, errMsg := classify(execCtx, runErr, stderr.String())
	w.finalize(run.ID, status, exitCode, errMsg)
	w.log.Info("run finished", "run_id", run.ID, "status", status)
}

func (w *Worker) heartbeatLoop(ctx context.Context, runID int64) {
	ticker := time.NewTicker(w.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.store.Heartbeat(ctx, runID, w.lease()); err != nil && ctx.Err() == nil {
				w.log.Warn("heartbeat failed", "run_id", runID, "err", err)
			}
		}
	}
}

// finalize records the terminal status using a fresh, short context so the
// result is always written even during shutdown.
func (w *Worker) finalize(runID int64, status string, exitCode *int, errMsg *string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.store.FinalizeRun(ctx, runID, status, exitCode, errMsg); err != nil {
		w.log.Error("finalize failed", "run_id", runID, "status", status, "err", err)
	}
}

// classify maps an exec result to a terminal status, exit code, and error message.
func classify(execCtx context.Context, runErr error, stderr string) (status string, exitCode *int, errMsg *string) {
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		return models.StatusTimedOut, ptrInt(-1), ptr("job exceeded timeout")
	}
	if runErr == nil {
		return models.StatusSucceeded, ptrInt(0), nil
	}
	code := -1
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		code = ee.ExitCode()
	}
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = runErr.Error()
	}
	return models.StatusFailed, &code, &msg
}

// sleep waits for d or until ctx is cancelled. Returns false if cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func ptr(s string) *string { return &s }
func ptrInt(i int) *int    { return &i }

// jobEnvAllowlist is the set of environment variables a job command inherits
// from the worker. Deliberately an allowlist, not a denylist, so a
// newly-introduced kronwrk secret in the worker's environment can never leak
// into a job by default. Notably excludes DATABASE_URL and every other
// kronwrk_* config var.
var jobEnvAllowlist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TZ", "TMPDIR", "LANG",
}

// jobEnv builds the environment for a job command: the allowlisted variables
// that are actually set in the worker's environment, plus every LC_* locale
// variable. Nothing else — in particular no kronwrk config or credentials.
func jobEnv() []string {
	var env []string
	for _, key := range jobEnvAllowlist {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}

// boundedBuffer captures up to a byte limit and silently discards the rest,
// always reporting a full write so the child process is never blocked.
type boundedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if remaining := maxOutputBytes - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + "\n[output truncated]"
	}
	return b.buf.String()
}
