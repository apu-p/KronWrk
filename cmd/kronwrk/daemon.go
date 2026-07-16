package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"kronwrk/internal/models"
)

// The daemon command manages the scheduler/worker as local background
// processes from inside the shell: status via pgrep, start by respawning this
// binary detached with the session's own credentials (admin and support pass
// both daemons' preflights per migration 0007), stop via SIGTERM for the
// usual graceful shutdown. This is deliberately machine-local — like the
// shell's offer to start Postgres via brew — not cluster orchestration;
// systemd/launchd deployments keep using `kronwrk scheduler|worker start`
// directly.

var daemonServices = []string{"scheduler", "worker"}

func daemonCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "daemon",
		Short: "Inspect and control the local scheduler and worker daemons",
	}
	c.AddCommand(daemonStatusCmd(), daemonStartCmd(), daemonStopCmd())
	return c
}

func daemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the scheduler and worker are running on this machine",
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := tableView{columns: []string{"SERVICE", "STATUS", "PID", "LOG"}}
			for _, svc := range daemonServices {
				pids := daemonPIDs(svc)
				status, pid := "up", joinPIDs(pids)
				if len(pids) == 0 {
					status, pid = "down", "-"
				}
				v.rows = append(v.rows, []string{svc, status, pid, daemonLogFile(svc)})
				v.dim = append(v.dim, len(pids) == 0)
			}
			return printTable(v)
		},
	}
}

func daemonStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <scheduler|worker>",
		Short: "Start a daemon in the background as the logged-in user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := daemonService(args[0])
			if err != nil {
				return err
			}
			if pids := daemonPIDs(svc); len(pids) > 0 {
				fmt.Printf("%s is already running (pid %s)\n", svc, joinPIDs(pids))
				return nil
			}

			cfg, _, err := getConfig()
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			logPath := daemonLogFile(svc)
			logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}

			child := exec.Command(exe, svc, "start")
			// The daemon connects as the logged-in user (its preflight verifies
			// the grants), not whatever DATABASE_URL the environment held.
			child.Env = append(os.Environ(), "DATABASE_URL="+cfg.DatabaseURL)
			child.Stdout, child.Stderr = logFile, logFile
			// Own process group so the shell's Ctrl-C never reaches the daemon.
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := child.Start(); err != nil {
				logFile.Close()
				return err
			}
			logFile.Close()
			// Audit who asked, over the operator's own session connection: the
			// daemon's start row shows the identity it runs as, not the actor.
			logRequested(cmd, svc, child.Process.Pid, models.EventStartRequested)

			// Preflight failures (wrong role, DB down) exit within a moment —
			// report them here with the log tail instead of claiming success.
			done := make(chan struct{})
			go func() { child.Wait(); close(done) }() //nolint:errcheck // exit status is in the log
			select {
			case <-done:
				return fmt.Errorf("%s exited during startup; last log lines:\n%s\n(full log: %s)",
					svc, tailFile(logPath, 5), logPath)
			case <-time.After(2 * time.Second):
				fmt.Printf("%s started (pid %d); logs: %s\n", svc, child.Process.Pid, logPath)
				return nil
			}
		},
	}
}

func daemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <scheduler|worker>",
		Short: "Gracefully stop a running daemon (SIGTERM)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := daemonService(args[0])
			if err != nil {
				return err
			}
			pids := daemonPIDs(svc)
			if len(pids) == 0 {
				fmt.Printf("%s is not running\n", svc)
				return nil
			}
			for _, pid := range pids {
				if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
					return fmt.Errorf("signal pid %d: %w", pid, err)
				}
				// The daemon's own stop row carries the identity it runs as; a
				// SIGTERM has no sender, so record the actor from this session.
				logRequested(cmd, svc, pid, models.EventStopRequested)
			}

			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				if len(daemonPIDs(svc)) == 0 {
					fmt.Printf("%s stopped\n", svc)
					return nil
				}
				time.Sleep(300 * time.Millisecond)
			}
			fmt.Printf("%s is still shutting down (in-flight runs finish first) — check `daemon status`\n", svc)
			return nil
		},
	}
}

// logRequested writes a *_requested audit row for the daemon at pid, stamped
// with the shell session's identity by LogServiceEvent's server-side
// current_user. The instance id matches the daemon's own hostname-pid rows so
// request and effect correlate. Best-effort: the action already happened, so
// an audit failure warns rather than failing the command.
func logRequested(cmd *cobra.Command, service string, pid int, event string) {
	ctx := cmd.Context()
	store, err := getStore(ctx)
	if err == nil {
		err = store.LogServiceEvent(ctx, service, instanceIDFor(pid), event)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record %s audit event: %v\n", event, err)
	}
}

// instanceIDFor renders the hostname-pid instance id the daemons use for
// their own service_events rows.
func instanceIDFor(pid int) string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, pid)
}

// daemonService validates a service argument.
func daemonService(name string) (string, error) {
	for _, svc := range daemonServices {
		if name == svc {
			return svc, nil
		}
	}
	return "", fmt.Errorf("unknown service %q (must be scheduler or worker)", name)
}

// daemonPIDs finds local processes running `kronwrk <service> start`,
// regardless of who started them (this shell, another shell, or by hand).
// Anchored to the end of the command line so wrapper shells whose command
// merely contains the invocation don't match — only the daemon itself.
func daemonPIDs(service string) []int {
	out, err := exec.Command("pgrep", "-f", fmt.Sprintf("kronwrk %s start$", service)).Output()
	if err != nil {
		return nil // pgrep exits 1 when nothing matches
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(field); err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	return pids
}

func joinPIDs(pids []int) string {
	s := make([]string, len(pids))
	for i, p := range pids {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

// daemonLogFile is where a shell-started daemon's output goes, alongside
// ~/.kronwrk_history.
func daemonLogFile(service string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".kronwrk_" + service + ".log"
	}
	return filepath.Join(home, ".kronwrk_"+service+".log")
}

// tailFile returns the last n lines of a file (dev-scale logs; read whole).
func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no log output)"
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
