package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/huh"

	"kronwrk/internal/db"
	"kronwrk/internal/schedule"
)

// wizardKeyMap extends huh's default keybindings for every kronwrk wizard:
//   - text inputs also move between fields with the arrow keys — ↑ previous,
//     ↓ next — on top of the existing shift+tab / tab / enter;
//   - Esc (as well as the default Ctrl+C) cancels the whole wizard, aborting
//     with huh.ErrUserAborted so callers can back out cleanly.
//
// Only the Input bindings gain ↑/↓; Select/Confirm keep their default ↑/↓
// (which move between options). The form checks Quit before the field, so Esc
// cancels reliably even inside a Select.
func wizardKeyMap() *huh.KeyMap {
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "cancel"))
	km.Input.Prev = key.NewBinding(
		key.WithKeys("shift+tab", "up"),
		key.WithHelp("↑/shift+tab", "prev field"),
	)
	km.Input.Next = key.NewBinding(
		key.WithKeys("enter", "tab", "down"),
		key.WithHelp("↓/enter", "next field"),
	)
	return km
}

// wizardHint is the one-line reminder printed above multi-field wizards so the
// navigation and cancel keys are discoverable without hunting for the footer.
func wizardHint() string {
	return faintText("  ↑/↓ or Tab/Shift+Tab move between fields · Enter on the last field saves · Esc to cancel")
}

// cancelHint is a compact reminder for single-step wizards that Esc backs out.
func cancelHint() string {
	return faintText("  Esc to cancel")
}

// stdinIsTTY reports whether stdin is an interactive terminal. Wizards and
// confirmations only run on a TTY; piped/scripted input keeps the strict
// flag-based behavior so automation never blocks on a prompt.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// nextRunsPreview renders the next n occurrences of a cron expression in the
// given timezone, one per line — the live feedback shown while typing a
// schedule in the job add wizard.
func nextRunsPreview(expr, timezone string, n int) string {
	if strings.TrimSpace(expr) == "" {
		return "5-field cron: minute hour day-of-month month day-of-week"
	}
	var b strings.Builder
	t := time.Now()
	for i := 0; i < n; i++ {
		next, err := schedule.NextRun(expr, timezone, t)
		if err != nil {
			return "invalid: " + err.Error()
		}
		fmt.Fprintf(&b, "next: %s\n", next.Format("Mon 2006-01-02 15:04 MST"))
		t = next
	}
	return strings.TrimRight(b.String(), "\n")
}

// jobAddWizard interactively fills the job add fields. Values already given
// as flags are shown pre-filled and stay editable.
func jobAddWizard(name, command, argsCSV, comment, scheduleExpr, timezone *string, timeout *int) error {
	timeoutStr := strconv.Itoa(*timeout)
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Job name").
			Value(name).
			Validate(nonEmpty("name")),
		huh.NewInput().
			Title("Command").
			Description("Executable to run (use an absolute path)").
			Value(command).
			Validate(nonEmpty("command")),
		huh.NewInput().
			Title("Arguments").
			Description("Comma-separated; leave empty for none").
			Value(argsCSV),
		huh.NewInput().
			Title("Comment").
			Description("Optional: change-request id or what this job is for").
			Value(comment),
		huh.NewInput().
			Title("Timezone").
			Description("IANA name the schedule is evaluated in").
			Value(timezone).
			Validate(func(s string) error {
				_, err := schedule.NextRun("* * * * *", s, time.Now())
				return err
			}),
		huh.NewInput().
			Title("Schedule").
			DescriptionFunc(func() string { return nextRunsPreview(*scheduleExpr, *timezone, 3) }, scheduleExpr).
			Value(scheduleExpr).
			Validate(func(s string) error {
				_, err := schedule.NextRun(s, *timezone, time.Now())
				return err
			}),
		huh.NewInput().
			Title("Timeout (seconds)").
			Description("0 = use the JOB_TIMEOUT_DEFAULT").
			Value(&timeoutStr).
			Validate(func(s string) error {
				n, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil || n < 0 {
					return fmt.Errorf("must be a non-negative integer")
				}
				return nil
			}),
	)).WithTheme(huh.ThemeBase16()).WithKeyMap(wizardKeyMap())
	fmt.Println(wizardHint())
	if err := form.Run(); err != nil {
		return err
	}
	*timeout, _ = strconv.Atoi(strings.TrimSpace(timeoutStr))
	return nil
}

// splitCSVArgs turns the wizard's comma-separated argument string into the
// same slice shape the --args flag produces.
func splitCSVArgs(s string) []string {
	var args []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			args = append(args, p)
		}
	}
	return args
}

// rolePickerWizard fills role via an arrow-key menu.
func rolePickerWizard(role *string) error {
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Role").
			Options(
				huh.NewOption("admin — full control: jobs, runs, daemons, migrations", "admin"),
				huh.NewOption("user — read-only: job list, run status", "user"),
				huh.NewOption("support — read + enable/disable jobs", "support"),
			).
			Value(role),
	)).WithTheme(huh.ThemeBase16()).WithKeyMap(wizardKeyMap()).Run()
}

// usernameWizard prompts for a username with the same validation as the CLI.
func usernameWizard(username *string) error {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Username").
			Validate(db.ValidateUsername).
			Value(username),
	)).WithTheme(huh.ThemeBase16()).WithKeyMap(wizardKeyMap()).Run()
}

// confirmWizard asks a yes/no question, defaulting to no.
func confirmWizard(title, affirmative string) (bool, error) {
	confirmed := false
	err := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title(title).
			Affirmative(affirmative).
			Negative("Cancel").
			Value(&confirmed),
	)).WithTheme(huh.ThemeBase16()).WithKeyMap(wizardKeyMap()).Run()
	return confirmed, err
}

// loginPrompt asks for a database username and password on entering the
// shell and rewrites the session's DATABASE_URL to use them, so the shell
// authenticates as whoever is sitting at the keyboard instead of silently
// trusting the OS user embedded in the default DATABASE_URL. Only runs on a
// TTY; piped/scripted invocations keep using DATABASE_URL as configured.
func loginPrompt() error {
	cfg, _, err := getConfig()
	if err != nil {
		return err
	}
	u, err := url.Parse(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("invalid DATABASE_URL: %w", err)
	}

	username := u.User.Username()
	fmt.Println(cancelHint())
	if err := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Username").
			Validate(nonEmpty("username")).
			Value(&username),
	)).WithTheme(huh.ThemeBase16()).WithKeyMap(wizardKeyMap()).Run(); err != nil {
		return err
	}

	password, err := promptExistingPassword()
	if err != nil {
		return err
	}

	u.User = url.UserPassword(username, password)
	cfg.DatabaseURL = u.String()
	sessionCfg = &cfg
	return nil
}

// promptNewPassword collects a password for a newly created account. On a TTY
// it uses a masked double-entry form (confirmation guards against typos in a
// password nobody has seen yet); with piped stdin it reads a single line
// (scripted use).
func promptNewPassword() (string, error) {
	if !stdinIsTTY() {
		return readPasswordLine()
	}

	var pw string
	err := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Password").
			EchoMode(huh.EchoModePassword).
			Validate(nonEmpty("password")).
			Value(&pw),
		huh.NewInput().
			Title("Confirm password").
			EchoMode(huh.EchoModePassword).
			Validate(func(s string) error {
				if s != pw {
					return fmt.Errorf("passwords do not match")
				}
				return nil
			}),
	)).WithTheme(huh.ThemeBase16()).WithKeyMap(wizardKeyMap()).Run()
	return pw, err
}

// promptExistingPassword collects a password for logging into an already-
// existing account — a single masked entry, no confirmation, since there's
// no typo to guard against.
func promptExistingPassword() (string, error) {
	if !stdinIsTTY() {
		return readPasswordLine()
	}

	var pw string
	err := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Password").
			EchoMode(huh.EchoModePassword).
			Validate(nonEmpty("password")).
			Value(&pw),
	)).WithTheme(huh.ThemeBase16()).WithKeyMap(wizardKeyMap()).Run()
	return pw, err
}

func readPasswordLine() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		return "", fmt.Errorf("password must not be empty")
	}
	return pw, nil
}

func nonEmpty(field string) func(string) error {
	return func(s string) error {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("%s must not be empty", field)
		}
		return nil
	}
}
