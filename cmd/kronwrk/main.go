// Command kronwrk is the scheduler, worker, and operator CLI in one binary.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spf13/cobra"

	"kronwrk/internal/db"
	"kronwrk/internal/models"
	"kronwrk/internal/scheduler"
	"kronwrk/internal/worker"
)

// errSilent signals that the failure has already been reported to the user in
// a friendly form (e.g. the login-failed message), so main should exit non-zero
// without printing the raw error on top of it.
var errSilent = errors.New("exit")

func main() {
	err := rootCmd().Execute()
	closeSession()
	if err != nil {
		if !errors.Is(err, errSilent) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "kronwrk",
		Short:         "A PostgreSQL-backed job scheduler, worker, and monitor",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Operator commands (job/run/user/whoami) only run inside the
		// interactive shell; daemons, migrate, and shell itself run directly
		// so systemd units and deploy scripts are unaffected. Inside the
		// shell, commands the connected role cannot use are refused up front
		// (cmdPerms) — presentation only; Postgres GRANTs still enforce.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if !requiresShell(cmd) {
				return nil
			}
			if !inShell {
				return fmt.Errorf("run `kronwrk shell` first — %q must be run from the interactive shell", cmd.CommandPath())
			}
			return authorizeShellCmd(cmd)
		},
		// Bare `kronwrk` on a terminal opens the interactive shell; with
		// piped stdin it prints help so scripts never hang on a prompt.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if stdinIsTTY() && !inShell {
				return runShell(cmd.Context())
			}
			return cmd.Help()
		},
	}
	root.AddCommand(migrateCmd(), jobCmd(), runCmd(), eventCmd(), schedulerCmd(), workerCmd(), userCmd(), whoamiCmd(), shellCmd(), daemonCmd(), monitorCmd())
	// Inside the shell, help for a command group (`job`, `help user`,
	// `job --help`) lists subcommands with the same role-aware dimming as the
	// shell's top-level `help`, instead of Cobra's default template. Leaf help
	// (`help job add`) keeps the default output with flags and usage.
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(c *cobra.Command, args []string) {
		if inShell && requiresShell(c) && c.HasSubCommands() {
			printGroupShellHelp(c)
			return
		}
		defaultHelp(c, args)
	})
	return root
}

// shellExempt lists top-level commands allowed to run directly, outside the
// interactive shell: long-running daemons (systemd needs to invoke these
// non-interactively), one-shot deploy setup, and the shell's own entrypoints.
var shellExempt = map[string]bool{
	"kronwrk":    true, // root: bare invocation / --help
	"scheduler":  true,
	"worker":     true,
	"migrate":    true,
	"shell":      true,
	"help":       true,
	"completion": true,
}

// requiresShell reports whether cmd must be run from inside `kronwrk shell`.
func requiresShell(cmd *cobra.Command) bool {
	return !shellExempt[topLevelCommand(cmd).Name()]
}

// topLevelCommand walks up to the command's direct child of root (or root
// itself), e.g. "job add" -> "job".
func topLevelCommand(cmd *cobra.Command) *cobra.Command {
	for cmd.Parent() != nil && cmd.Parent().Parent() != nil {
		cmd = cmd.Parent()
	}
	return cmd
}

// --- shared helpers ---

// signalContext cancels on SIGINT/SIGTERM for graceful shutdown.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// --- migrate ---

func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply database schema migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, log, err := getConfig()
			if err != nil {
				return err
			}
			if err := db.RunMigrations(cfg.DatabaseURL); err != nil {
				return err
			}
			log.Info("migrations applied")
			return nil
		},
	}
}

// --- job ---

func jobCmd() *cobra.Command {
	c := &cobra.Command{Use: "job", Short: "Manage job definitions"}
	c.AddCommand(jobAddCmd(), jobListCmd(), jobDisableCmd(), jobEnableCmd(), jobConditionCmd())
	return c
}

func jobAddCmd() *cobra.Command {
	var (
		name, command, scheduleExpr, timezone, comment string
		args                                           []string
		timeout                                        int
	)
	c := &cobra.Command{
		Use:   "add",
		Short: "Create a job definition (interactive wizard when flags are omitted)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" || command == "" || scheduleExpr == "" {
				if !stdinIsTTY() {
					return fmt.Errorf("missing required flags: --name, --command, --schedule")
				}
				argsCSV := strings.Join(args, ",")
				if err := jobAddWizard(&name, &command, &argsCSV, &comment, &scheduleExpr, &timezone, &timeout); err != nil {
					return err
				}
				args = splitCSVArgs(argsCSV)
			}
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			job, err := store.InsertJob(ctx, models.Job{
				Name:           name,
				Command:        command,
				Args:           args,
				ScheduleExpr:   scheduleExpr,
				Timezone:       timezone,
				TimeoutSeconds: timeout,
				Comment:        strings.TrimSpace(comment),
			})
			if err != nil {
				return permissionErr(ctx, store, err, "create jobs", "the admin role")
			}
			fmt.Printf("created job %d (%s); next run at %s\n", job.ID, job.Name, fmtTime(job.NextRunAt))
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "job name (required)")
	c.Flags().StringVar(&command, "command", "", "command to execute (required)")
	c.Flags().StringSliceVar(&args, "args", nil, "command arguments")
	c.Flags().StringVar(&scheduleExpr, "schedule", "", "5-field cron expression (required)")
	c.Flags().StringVar(&timezone, "timezone", localTimezone(), "IANA timezone for the schedule (defaults to the system timezone)")
	c.Flags().IntVar(&timeout, "timeout", 0, "per-run timeout in seconds (0 = use default)")
	c.Flags().StringVar(&comment, "comment", "", "optional note: change-request id or what the job is for")
	return c
}

func jobListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List job definitions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			jobs, err := store.ListJobs(ctx)
			if err != nil {
				return err
			}
			if len(jobs) == 0 && stdoutIsTTY() {
				fmt.Println(faintText("No jobs yet — create one with `job add`."))
				return nil
			}
			return printTable(jobsTableView(jobs, time.Now()))
		},
	}
}

// jobsTableView shapes jobs for display: preformatted cells, disabled jobs
// flagged for the faint shade. Kept separate from rendering (and pure — now is
// a parameter) so an interactive table can reuse it as-is.
func jobsTableView(jobs []models.Job, now time.Time) tableView {
	v := tableView{columns: []string{"ID", "NAME", "SCHEDULE", "TIMEZONE", "ENABLED", "NEXT RUN", "COMMENT"}}
	for _, j := range jobs {
		v.rows = append(v.rows, []string{
			strconv.FormatInt(j.ID, 10),
			j.Name,
			j.ScheduleExpr,
			j.Timezone,
			map[bool]string{true: "yes", false: "no"}[j.Enabled],
			fmtNextRun(j, now),
			truncate(j.Comment, 40),
		})
		v.dim = append(v.dim, !j.Enabled)
	}
	return v
}

// fmtNextRun shows a job's next run in its own timezone with a relative hint,
// e.g. "2026-07-04 02:00 IST (in 7h12m)".
func fmtNextRun(j models.Job, now time.Time) string {
	if j.NextRunAt == nil {
		return "-"
	}
	t := *j.NextRunAt
	if loc, err := time.LoadLocation(j.Timezone); err == nil {
		t = t.In(loc)
	}
	return fmt.Sprintf("%s (%s)", t.Format("2006-01-02 15:04 MST"), relTime(*j.NextRunAt, now))
}

func jobDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <id>",
		Short: "Mark a job inactive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			id, err := strconv.ParseInt(cmdArgs[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id: %w", err)
			}
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			if err := store.DisableJob(ctx, id); err != nil {
				return permissionErr(ctx, store, err, "disable jobs", "the support or admin role")
			}
			fmt.Printf("disabled job %d\n", id)
			return nil
		},
	}
}

func jobEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <id>",
		Short: "Re-activate a disabled job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			id, err := strconv.ParseInt(cmdArgs[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id: %w", err)
			}
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			if err := store.EnableJob(ctx, id); err != nil {
				return permissionErr(ctx, store, err, "enable jobs", "the support or admin role")
			}
			fmt.Printf("enabled job %d\n", id)
			return nil
		},
	}
}

// --- run ---

func runCmd() *cobra.Command {
	c := &cobra.Command{Use: "run", Short: "Inspect job runs"}
	c.AddCommand(runStatusCmd())
	return c
}

func runStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <run-id>",
		Short: "Show status and details for a run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			id, err := strconv.ParseInt(cmdArgs[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid run id: %w", err)
			}
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			r, err := store.GetRun(ctx, id)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "run id:\t%d\n", r.ID)
			fmt.Fprintf(tw, "job id:\t%d\n", r.JobID)
			fmt.Fprintf(tw, "job name:\t%s\n", r.JobName)
			fmt.Fprintf(tw, "status:\t%s\n", r.Status)
			fmt.Fprintf(tw, "scheduled for:\t%s\n", r.ScheduledFor.Format(time.RFC3339))
			fmt.Fprintf(tw, "wait deadline:\t%s\n", fmtTime(r.WaitDeadline))
			fmt.Fprintf(tw, "attempt:\t%d\n", r.Attempt)
			fmt.Fprintf(tw, "worker:\t%s\n", deref(r.WorkerID))
			fmt.Fprintf(tw, "started:\t%s\n", fmtTime(r.StartedAt))
			fmt.Fprintf(tw, "finished:\t%s\n", fmtTime(r.FinishedAt))
			fmt.Fprintf(tw, "exit code:\t%s\n", fmtInt(r.ExitCode))
			fmt.Fprintf(tw, "error:\t%s\n", deref(r.ErrorMessage))
			return tw.Flush()
		},
	}
}

// --- scheduler / worker ---

func schedulerCmd() *cobra.Command {
	c := &cobra.Command{Use: "scheduler", Short: "Run the scheduler"}
	c.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Start the scheduling loop",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, log, err := getConfig()
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}
			if err := store.PreflightScheduler(ctx); err != nil {
				return permissionErr(ctx, store, err, "schedule runs", "the scheduler, operator, or admin role")
			}
			return scheduler.New(store, cfg.PollInterval, log).Run(ctx)
		},
	})
	return c
}

func workerCmd() *cobra.Command {
	c := &cobra.Command{Use: "worker", Short: "Run a worker"}
	c.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Start a worker that executes queued runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, log, err := getConfig()
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}
			if err := store.PreflightWorker(ctx); err != nil {
				return permissionErr(ctx, store, err, "claim and execute runs", "the worker, operator, or admin role")
			}
			w := worker.New(store, cfg.WorkerConcurrency, cfg.PollInterval, cfg.HeartbeatInterval, cfg.JobTimeoutDefault, log)
			return w.Run(ctx)
		},
	})
	return c
}

// --- user management ---

func userCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "user",
		Short: "Manage database users and their roles",
		Long: "Manage the Postgres login roles that control access to Kronwrk.\n" +
			"These commands need a privileged DATABASE_URL (CREATEROLE, e.g. the\n" +
			"bootstrap superuser); access itself is enforced by Postgres GRANTs.",
	}
	c.AddCommand(userAddCmd(), userListCmd(), userRemoveCmd(), userSetRoleCmd())
	return c
}

func userAddCmd() *cobra.Command {
	var role, password string
	c := &cobra.Command{
		Use:   "add [username]",
		Short: "Create a database user with a single role (interactive when omitted)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			var username string
			if len(cmdArgs) == 1 {
				username = cmdArgs[0]
			} else if stdinIsTTY() {
				if err := usernameWizard(&username); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("missing username argument")
			}
			if err := db.ValidateUsername(username); err != nil {
				return err
			}
			if role == "" && stdinIsTTY() {
				if err := rolePickerWizard(&role); err != nil {
					return err
				}
			}
			if _, ok := db.GroupRoles[role]; !ok {
				return fmt.Errorf("unknown role %q (valid roles: %s)", role, strings.Join(db.RoleNames(), ", "))
			}
			if password == "" {
				pw, err := promptNewPassword()
				if err != nil {
					return err
				}
				password = pw
			}
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			if err := store.CreateUser(ctx, username, password, role); err != nil {
				return permissionErr(ctx, store, err, "create users", "a CREATEROLE connection (e.g. the bootstrap superuser)")
			}
			cfg, _, _ := getConfig()
			fmt.Printf("created user %q with role %s\n", username, role)
			fmt.Printf("connect with: DATABASE_URL=%s\n", connectHint(cfg.DatabaseURL, username))
			return nil
		},
	}
	c.Flags().StringVar(&role, "role", "", fmt.Sprintf("role to assign: %s", strings.Join(db.RoleNames(), ", ")))
	c.Flags().StringVar(&password, "password", "", "password (discouraged: leaks into shell history; omit to be prompted)")
	return c
}

func userListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List database users and their roles",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			users, err := store.ListUsers(ctx)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "USERNAME\tROLE")
			for _, u := range users {
				fmt.Fprintf(tw, "%s\t%s\n", u.Username, fmtRoles(u.Roles))
			}
			return tw.Flush()
		},
	}
}

func userRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <username>",
		Short: "Remove a database user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			if stdinIsTTY() {
				ok, err := confirmWizard(
					fmt.Sprintf("Remove user %q? This drops their database role.", cmdArgs[0]),
					"Remove")
				if err != nil {
					return err
				}
				if !ok {
					fmt.Println("cancelled")
					return nil
				}
			}
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			if err := store.DropUser(ctx, cmdArgs[0]); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "2BP01" { // dependent_objects_still_exist
					return fmt.Errorf("%w\nhint: the user owns database objects; reassign them first (REASSIGN OWNED BY %s TO ...; DROP OWNED BY %s)",
						err, cmdArgs[0], cmdArgs[0])
				}
				return permissionErr(ctx, store, err, "remove users", "a CREATEROLE connection (e.g. the bootstrap superuser)")
			}
			fmt.Printf("removed user %q\n", cmdArgs[0])
			return nil
		},
	}
}

func userSetRoleCmd() *cobra.Command {
	var role string
	c := &cobra.Command{
		Use:   "set-role <username>",
		Short: "Reassign a user to a different role (a user holds exactly one)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			if role == "" && stdinIsTTY() {
				if err := rolePickerWizard(&role); err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			if err := store.SetUserRole(ctx, cmdArgs[0], role); err != nil {
				return permissionErr(ctx, store, err, "change user roles", "a CREATEROLE connection (e.g. the bootstrap superuser)")
			}
			fmt.Printf("user %q now has role %s\n", cmdArgs[0], role)
			return nil
		},
	}
	c.Flags().StringVar(&role, "role", "", fmt.Sprintf("role to assign: %s", strings.Join(db.RoleNames(), ", ")))
	return c
}

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the connected database user and Kronwrk role",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}

			u, err := store.WhoAmI(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("connected as: %s\n", u.Username)
			fmt.Printf("kronwrk role: %s\n", roleDisplay(u))
			return nil
		},
	}
}

// connectHint renders the DATABASE_URL a newly created user would connect
// with: the session's actual host/port/database with the new username swapped
// in and the session's password stripped — not a hardcoded localhost URL.
func connectHint(databaseURL, username string) string {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return fmt.Sprintf("postgres://%s@<host>:<port>/<db>", username)
	}
	u.User = url.User(username)
	return u.String()
}

// roleDisplay renders a user's kronwrk role for display, with fallbacks for
// logins that hold no group role (superusers bypass GRANTs entirely).
func roleDisplay(u models.DBUser) string {
	if role := fmtRoles(u.Roles); role != "" {
		return role
	}
	if u.Superuser {
		return "superuser (unrestricted)"
	}
	return "none"
}

// fmtRoles renders kronwrk group memberships under their CLI names. More
// than one role violates the one-role-per-user rule, so flag it.
func fmtRoles(groups []string) string {
	names := make([]string, len(groups))
	for i, g := range groups {
		names[i] = db.CLIRoleFor(g)
	}
	s := strings.Join(names, ", ")
	if len(names) > 1 {
		s += " (!) multiple roles; fix with `kronwrk user set-role`"
	}
	return s
}

// permissionErr maps insufficient_privilege (42501) to a short actionable
// message naming the connected role; other errors pass through unchanged.
func permissionErr(ctx context.Context, store *db.Store, err error, action, needs string) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		return err
	}
	who := "your role"
	if u, whoErr := store.WhoAmI(ctx); whoErr == nil {
		who = fmt.Sprintf("role %q", u.Username)
	}
	return fmt.Errorf("permission denied: %s cannot %s (requires %s; see `kronwrk whoami`)", who, action, needs)
}

// --- formatting helpers ---

// localTimezone returns the system's IANA timezone name (e.g. "Asia/Kolkata").
// time.Local.String() reports "Local" on macOS rather than the zone name, so we
// resolve it from the TZ env var and the /etc/localtime symlink, falling back to
// UTC only if nothing else works.
func localTimezone() string {
	// TZ env var wins if it names a loadable zone.
	if tz := os.Getenv("TZ"); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	// Resolve /etc/localtime → .../zoneinfo/<Area>/<Zone> (macOS and Linux).
	if path, err := os.Readlink("/etc/localtime"); err == nil {
		const marker = "zoneinfo/"
		if idx := strings.LastIndex(path, marker); idx != -1 {
			name := path[idx+len(marker):]
			if _, err := time.LoadLocation(name); err == nil {
				return name
			}
		}
	}
	if name := time.Local.String(); name != "" && name != "Local" {
		return name
	}
	return "UTC"
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func fmtInt(i *int) string {
	if i == nil {
		return "-"
	}
	return strconv.Itoa(*i)
}

func deref(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}
