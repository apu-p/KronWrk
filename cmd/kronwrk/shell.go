package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"github.com/chzyer/readline"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"kronwrk/internal/models"
)

// inShell guards against `shell` being run from inside the shell.
var inShell bool

func shellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell",
		Short: "Interactive session with history and tab-completion",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if inShell {
				return fmt.Errorf("already in a shell")
			}
			return runShell(cmd.Context())
		},
	}
}

// runShell reads commands in a loop and dispatches them to a fresh Cobra tree
// per line (rootCmd is a constructor, so flag state never leaks between
// lines). The session's shared pool keeps every command on one connection.
// The login prompt lives here (not in shellCmd) so every shell entry point —
// `kronwrk shell` and bare `kronwrk` on a TTY — authenticates the same way.
func runShell(ctx context.Context) error {
	if stdinIsTTY() {
		clearScreen()
		printBannerArt()
		if err := ensurePostgresRunning(); err != nil {
			// Esc/Ctrl+C at the "start Postgres?" prompt = quit, not an error.
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
		if err := authenticate(ctx); err != nil {
			// Esc/Ctrl+C at the login prompt = the user backing out of the shell.
			if errors.Is(err, huh.ErrUserAborted) {
				return nil
			}
			return err
		}
	} else if err := refuseSuperuser(ctx); err != nil {
		// Piped/scripted: no login prompt, but still refuse a superuser URL.
		return err
	}

	inShell = true
	defer func() { inShell = false }()

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          shellPrompt(ctx),
		HistoryFile:     historyFile(),
		AutoComplete:    buildCompleter(),
		InterruptPrompt: "^C",
		// History is saved manually (below) so secrets can be redacted
		// before a line is persisted to disk.
		DisableAutoSaveHistory: true,
	})
	if err != nil {
		return err
	}
	defer rl.Close()

	if stdinIsTTY() {
		printConnectionInfo(ctx)
	}
	for {
		// Re-check and print daemon status before every prompt so the line
		// directly above the input area is always current — including right
		// after a `daemon start/stop` on the previous line.
		if stdinIsTTY() {
			fmt.Println(daemonStatusLine(ctx))
		}
		line, err := rl.Readline()
		if err == readline.ErrInterrupt {
			continue // Ctrl-C clears the current line
		}
		if err != nil {
			return nil // Ctrl-D / EOF
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rl.SaveHistory(redactSecrets(line)) //nolint:errcheck // history is best-effort
		if line == "exit" || line == "quit" {
			return nil
		}
		if line == "logout" {
			if !stdinIsTTY() {
				fmt.Fprintln(os.Stderr, "error: logout is only available in an interactive shell")
				continue
			}
			if !logout(ctx, rl) {
				return nil // logged out and chose to leave (or re-login failed)
			}
			continue
		}

		args, err := splitArgs(line)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}
		// Forgive a leading "kronwrk" (muscle memory from the one-shot CLI).
		if args[0] == "kronwrk" {
			args = args[1:]
			if len(args) == 0 {
				continue
			}
		}
		// Bare `help`/`?` shows shell-tailored help; `help <cmd>` still falls
		// through to Cobra for per-command detail.
		if len(args) == 1 && (args[0] == "help" || args[0] == "?") {
			printShellHelp(ctx)
			continue
		}

		root := rootCmd()
		root.SetArgs(args)
		if err := root.ExecuteContext(ctx); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				// The user pressed Esc/Ctrl+C inside a wizard — a normal cancel,
				// not an error. Nothing was committed.
				fmt.Println(faintText("cancelled"))
			} else {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		}
	}
}

// authenticate runs the login prompt, opens the shared pool as the entered
// user (surfacing a bad password as the friendly login-failed message), and
// refuses superuser logins. Shared by initial shell entry and `logout` so both
// paths authenticate identically.
func authenticate(ctx context.Context) error {
	if err := loginPrompt(); err != nil {
		return err
	}
	// Open the pool now so a bad username/password surfaces here, as a friendly
	// login message, instead of a raw pgx auth error later.
	if _, err := getStore(ctx); err != nil {
		if isAuthError(err) {
			printLoginFailed()
			return errSilent
		}
		return err
	}
	return refuseSuperuser(ctx)
}

// logout drops the authenticated session and returns the shell to its
// pre-login state: the user either logs in again or leaves. It returns true
// when the session was re-authenticated (stay in the REPL) and false when the
// shell should exit.
func logout(ctx context.Context, rl *readline.Instance) bool {
	closeSession()            // release the old connection/pool
	clearSessionCredentials() // next login starts from a blank username
	printLoggedOut()

	again, err := confirmWizard("Log in again?", "Log in")
	if err != nil || !again {
		printGoodbye()
		return false
	}
	if err := authenticate(ctx); err != nil {
		// errSilent already showed the login-failed box; an aborted prompt is
		// the user choosing to leave. Anything else (e.g. superuser refusal) is
		// worth printing before we exit.
		if !errors.Is(err, errSilent) && !errors.Is(err, huh.ErrUserAborted) {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		return false
	}
	rl.SetPrompt(shellPrompt(ctx))
	printConnectionInfo(ctx)
	return true
}

// clearSessionCredentials strips the username/password from the session's
// DATABASE_URL so the next login prompt starts blank instead of pre-filling the
// just-logged-out identity.
func clearSessionCredentials() {
	if sessionCfg == nil {
		return
	}
	if u, err := url.Parse(sessionCfg.DatabaseURL); err == nil {
		u.User = nil
		sessionCfg.DatabaseURL = u.String()
	}
}

// printLoggedOut renders the styled confirmation shown after `logout`, before
// the log-in-again-or-exit choice.
func printLoggedOut() {
	fmt.Println()
	fmt.Println(accentBox(accentText("Logged out")))
	fmt.Println(faintText("  Your session is closed. Log in again to continue, or exit."))
}

// printGoodbye is the parting line shown when the user declines to log in again.
func printGoodbye() {
	fmt.Println(faintText("  Goodbye."))
}

// postgresService is the Homebrew service the shell offers to start when
// Postgres is down — matching this project's documented dev setup (keg-only
// postgresql@17, same pin as pg.sh).
const postgresService = "postgresql@17"

// ensurePostgresRunning checks that the configured database host/port is
// accepting TCP connections before the login prompt asks for credentials
// against it — otherwise a down server just looks like a connection error
// after typing a password. If it's down, offers to start the local Homebrew
// service (matches this project's documented dev setup) rather than failing
// outright.
func ensurePostgresRunning() error {
	cfg, _, err := getConfig()
	if err != nil {
		return err
	}
	if postgresReachable(cfg.DatabaseURL) {
		return nil
	}
	// The offer-to-start is Homebrew-specific (this project's documented dev
	// setup); on hosts without brew, don't offer what we can't do.
	if _, err := exec.LookPath("brew"); err != nil {
		return fmt.Errorf("Postgres is not running — start it and re-run `kronwrk shell`")
	}

	start, err := confirmWizard("Postgres doesn't seem to be running. Start it now?", "Start")
	if err != nil {
		return err
	}
	if !start {
		return fmt.Errorf("Postgres is not running — start it and re-run `kronwrk shell`")
	}

	fmt.Println("Starting " + postgresService + " via brew services...")
	cmd := exec.Command("brew", "services", "start", postgresService)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start Postgres: %w", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if postgresReachable(cfg.DatabaseURL) {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("Postgres did not become reachable within 10s of starting")
}

// postgresReachable reports whether something is accepting TCP connections at
// the configured database host/port. It only checks connectivity, not
// authentication — this runs before login, when credentials aren't known yet.
func postgresReachable(databaseURL string) bool {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		host = "localhost"
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// isAuthError reports whether err is a Postgres authentication failure — a
// wrong password (28P01) or an invalid/nonexistent role (28000). Postgres
// deliberately returns the same class for "bad password" and "no such user",
// so the friendly message must not distinguish them either.
func isAuthError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "28P01" || pgErr.Code == "28000"
}

// printLoginFailed renders the styled login-failure message shown just before
// the shell exits on bad credentials.
func printLoginFailed() {
	fmt.Println(accentBox(accentText("Login failed")))
	fmt.Println(faintText("  Wrong username or password. Run `kronwrk shell` to try again."))
}

// refuseSuperuser blocks the interactive shell from operating as a Postgres
// superuser. Superusers bypass every RBAC GRANT, so a shell session as one is
// unrestricted regardless of the role model — and over a trust socket the login
// password isn't even verified. Superuser access is intentionally confined to
// direct, non-shell paths: `migrate`, psql, and external tools like DBeaver.
// User management stays possible in the shell via the non-superuser user-admin
// login (a person-named kronwrk_admin + CREATEROLE role), not the superuser.
func refuseSuperuser(ctx context.Context) error {
	u, err := currentUser(ctx)
	if err != nil {
		return err
	}
	if u.Superuser {
		return fmt.Errorf("refusing to open the shell as superuser %q: superusers bypass RBAC.\n"+
			"Use a role-scoped login here (see `user add`), or the user-admin login to manage users.\n"+
			"Superuser access is for `migrate`, psql, and DBeaver only.", u.Username)
	}
	return nil
}

// printShellHelp prints help tailored to the interactive shell: only the
// operator commands that actually run here (daemons, migrate, and the shell
// entrypoints are direct-invocation only and excluded via requiresShell) plus
// the shell built-ins. Derived from the live command tree so new operator
// commands appear automatically. Commands the connected role cannot run
// (cmdPerms) render entirely in the faint shade, with what they require —
// mirroring the up-front refusal in authorizeShellCmd.
func printShellHelp(ctx context.Context) {
	u, uErr := currentUser(ctx) // on error, skip dimming; the DB still enforces
	fmt.Println(accentText("Commands"))
	for _, c := range rootCmd().Commands() {
		if c.Hidden || !requiresShell(c) {
			continue
		}
		printLeafLines(u, uErr == nil, c)
	}

	fmt.Println()
	fmt.Println(accentText("Shell"))
	fmt.Printf("  %-14s %s\n", "help", faintText("show this help; `help <command>` for command detail"))
	fmt.Printf("  %-14s %s\n", "logout", faintText("end this session and log in as someone else, or exit"))
	fmt.Printf("  %-14s %s\n", "exit", faintText("leave the shell (also `quit` or Ctrl-D)"))
	fmt.Println()
	fmt.Println(faintText("Tab-completes commands and flags. No `kronwrk` prefix needed."))
	fmt.Println(faintText("Faint commands are outside your role's access."))
}

// printGroupShellHelp renders shell help for one command group (`job`,
// `user`, ...): its leaves with the same role-aware dimming as the top-level
// `help`. Installed as the Cobra help function for parent commands inside the
// shell, so bare `job`, `help job`, and `job --help` all get it.
func printGroupShellHelp(c *cobra.Command) {
	u, uErr := currentUser(c.Context())
	fmt.Println(accentText(c.Short))
	printLeafLines(u, uErr == nil, c)
	fmt.Println(faintText(fmt.Sprintf("Faint commands are outside your role's access. `help %s <subcommand>` for detail.", c.Name())))
}

// printLeafLines prints one help line per leaf under c, rendering leaves the
// connected role cannot run entirely in the faint shade with what they
// require. roleKnown false (currentUser failed) disables dimming rather than
// dimming everything — the DB still enforces.
func printLeafLines(u models.DBUser, roleKnown bool, c *cobra.Command) {
	for _, leaf := range shellLeaves(c) {
		path := shellPath(leaf)
		if allowed, requires := allowedFor(u, path); roleKnown && !allowed {
			fmt.Println(faintText(fmt.Sprintf("  %-14s %s — requires %s", path, leaf.Short, requires)))
			continue
		}
		fmt.Printf("  %-14s %s\n", path, faintText(leaf.Short))
	}
}

// shellLeaves flattens a command into the runnable leaves shown in shell help:
// the command itself when it has no subcommands (whoami), otherwise its
// visible subcommands (job -> job add, job list, ...).
func shellLeaves(c *cobra.Command) []*cobra.Command {
	subs := c.Commands()
	if len(subs) == 0 {
		return []*cobra.Command{c}
	}
	var leaves []*cobra.Command
	for _, sub := range subs {
		if sub.Hidden {
			continue
		}
		leaves = append(leaves, shellLeaves(sub)...)
	}
	return leaves
}

// printBanner renders the interactive-shell welcome: a bordered title box plus
// a line naming the connected role. TTY-gated by the caller so piped/scripted
// use stays free of decoration. Reuses WhoAmI (as shellPrompt does) so the
// banner and prompt always agree on who you are.
// asciiTitle is a hand-built block-font "KRONWRK" — pasted static rather than
// generated at runtime (no figlet dep) since it never changes.
const asciiTitle = `██   ██  ██████    █████   ██   ██  ██   ██  ██████   ██   ██
██  ██   ██   ██  ██   ██  ███  ██  ██   ██  ██   ██  ██  ██
██ ██    ██   ██  ██   ██  ████ ██  ██   ██  ██   ██  ██ ██
████     ██████   ██   ██  ██ ████  ██ █ ██  ██████   ████
██ ██    ██ ██    ██   ██  ██  ███  ██ █ ██  ██ ██    ██ ██
██  ██   ██  ██   ██   ██  ██   ██  ███ ███  ██  ██   ██  ██
██   ██  ██   ██   █████   ██   ██   ██ ██   ██   ██  ██   ██`

// clearScreen wipes the terminal and homes the cursor (what `clear` does), so
// the shell opens on a clean screen with the banner at the top. TTY-only —
// callers gate on stdinIsTTY so piped output never contains control codes.
func clearScreen() {
	fmt.Print("\x1b[2J\x1b[H")
}

// printBannerArt renders the welcome box across the full terminal width, with
// the title centered. Falls back to a plain wordmark when the window is too
// narrow for the block-font art (which is ~62 columns wide).
func printBannerArt() {
	w := terminalWidth()
	title := accentText(asciiTitle)
	if w < 70 {
		title = accentText("K R O N W R K")
	}
	subtitle := faintText("Crontab with additional features · interactive shell")
	inner := lipgloss.JoinVertical(lipgloss.Center, title, "", subtitle)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accentColor).
		Padding(2, 0).
		Width(w - 2). // content width; the border brings the box to w
		Align(lipgloss.Center).
		Render(inner)
	fmt.Println(box)
}

// terminalWidth is the current terminal width in columns, defaulting to 80
// when it cannot be determined (the pre-resize-aware banner's old assumption).
func terminalWidth() int {
	if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
		return w
	}
	return 80
}

func printConnectionInfo(ctx context.Context) {
	conn := "not connected"
	if u, err := currentUser(ctx); err == nil {
		conn = fmt.Sprintf("%s(%s)", u.Username, roleDisplay(u))
	}
	fmt.Println(faintText("  Connected as ") + conn)
	fmt.Println(faintText("  Type 'help' · tab to complete · 'exit' to leave"))
}

// daemonStatusLine is the one-line daemon summary printed above the input
// area before every prompt: whether the scheduler and worker are up on this
// machine, with a control hint only for roles allowed to start/stop them
// (cmdPerms).
func daemonStatusLine(ctx context.Context) string {
	parts := make([]string, len(daemonServices))
	for i, svc := range daemonServices {
		if pids := daemonPIDs(svc); len(pids) > 0 {
			parts[i] = svc + " " + statusUp()
		} else {
			parts[i] = svc + " " + statusDown()
		}
	}
	line := faintText("Daemons: ") + strings.Join(parts, faintText(" · "))
	if u, err := currentUser(ctx); err == nil {
		if ok, _ := allowedFor(u, "daemon start"); ok {
			line += faintText(" — `daemon start|stop <name>`")
		}
	}
	return line
}

// shellPrompt shows who the session is connected as, e.g. "achu(admin) ▸ ".
func shellPrompt(ctx context.Context) string {
	u, err := currentUser(ctx)
	if err != nil {
		return "kronwrk ▸ "
	}
	return fmt.Sprintf("%s(%s) ▸ ", u.Username, roleDisplay(u))
}

// passwordFlagRE matches a --password flag and its value, in both
// `--password pw` and `--password=pw` forms, including quoted values.
var passwordFlagRE = regexp.MustCompile(`(--password)(?:=|\s+)(?:'[^']*'|"[^"]*"|\S+)`)

// redactSecrets strips secret values from a shell line before it is written
// to the history file, so `user add ... --password pw` never persists the
// password to disk.
func redactSecrets(line string) string {
	return passwordFlagRE.ReplaceAllString(line, "$1 [redacted]")
}

// historyFile returns the shell-history path, ensuring the file itself is
// private: history could hold operational details (and, before redaction,
// held passwords), so it must not be world-readable. The chmod also repairs
// files created 0644 by earlier builds.
func historyFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".kronwrk_history")
	if f, err := os.OpenFile(path, os.O_CREATE, 0o600); err == nil {
		f.Close()
	}
	_ = os.Chmod(path, 0o600)
	return path
}

// buildCompleter derives tab-completion from the Cobra tree itself, so new
// commands and flags complete without touching this file.
func buildCompleter() *readline.PrefixCompleter {
	items := completerItems(rootCmd())
	items = append(items, readline.PcItem("logout"), readline.PcItem("exit"), readline.PcItem("quit"))
	return readline.NewPrefixCompleter(items...)
}

func completerItems(c *cobra.Command) []readline.PrefixCompleterInterface {
	var items []readline.PrefixCompleterInterface
	for _, sub := range c.Commands() {
		// Only shell-relevant commands: skips daemons, migrate, shell, and
		// completion (all direct-invocation only) — matching printShellHelp.
		if sub.Hidden || !requiresShell(sub) {
			continue
		}
		children := completerItems(sub)
		sub.Flags().VisitAll(func(f *pflag.Flag) {
			children = append(children, readline.PcItem("--"+f.Name))
		})
		items = append(items, readline.PcItem(sub.Name(), children...))
	}
	return items
}

// splitArgs splits a shell line into arguments, honoring single quotes,
// double quotes, and backslash escapes outside single quotes.
func splitArgs(line string) ([]string, error) {
	var (
		args           []string
		cur            strings.Builder
		inWord         bool
		single, double bool
		escaped        bool
	)
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && !single:
			escaped = true
			inWord = true
		case r == '\'' && !double:
			single = !single
			inWord = true
		case r == '"' && !single:
			double = !double
			inWord = true
		case (r == ' ' || r == '\t') && !single && !double:
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if escaped {
		return nil, fmt.Errorf("trailing backslash")
	}
	if single || double {
		return nil, fmt.Errorf("unclosed quote")
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}
