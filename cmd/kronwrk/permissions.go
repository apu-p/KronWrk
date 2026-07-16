package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"kronwrk/internal/db"
	"kronwrk/internal/models"
)

// Role-aware presentation for the shell: commands the connected role cannot
// use are dimmed in `help` and refused before dispatch with a friendly
// message. This is UX only — Postgres GRANTs remain the enforcement layer,
// and permissionErr still maps 42501 if this matrix ever drifts from the
// migrations.

// perm describes who a shell command is available to. The zero value means
// open to everyone (as does absence from cmdPerms).
type perm struct {
	roles      []string // CLI role names whose group GRANTs cover the command
	createRole bool     // needs the CREATEROLE attribute (the user-admin login), not a group role
}

// cmdPerms mirrors the GRANT matrix from migrations 0003–0014, keyed by the
// command path as typed in the shell. Keep in sync when a migration changes a
// role's grants.
var cmdPerms = map[string]perm{
	"job add":  {roles: []string{"admin"}},
	"job list": {roles: []string{"admin", "user", "support", "scheduler", "worker", "operator"}},
	// scheduler and operator hold table-level UPDATE on jobs (the scheduler
	// preflight's SELECT ... FOR UPDATE needs it), so enable/disable
	// genuinely pass their GRANTs.
	"job disable": {roles: []string{"admin", "support", "scheduler", "operator"}},
	"job enable":  {roles: []string{"admin", "support", "scheduler", "operator"}},
	// Conditions gate scheduling, so add/remove is admin-only like job add;
	// listing follows SELECT on job_conditions (0014 — scheduler included,
	// its promotion pass reads them).
	"job condition add":    {roles: []string{"admin"}},
	"job condition remove": {roles: []string{"admin"}},
	"job condition list":   {roles: []string{"admin", "user", "support", "scheduler", "operator"}},
	// scheduler only INSERTs into job_runs — the one role that cannot read runs,
	// which also rules it out of the monitor's jobs⋈job_runs view.
	"run status": {roles: []string{"admin", "user", "support", "worker", "operator"}},
	"monitor":    {roles: []string{"admin", "user", "support", "worker", "operator"}},
	// Emitting events is an operational act (it releases waiting runs):
	// admin + operator, per 0014's INSERT grant. Listing follows SELECT on
	// events; the scheduler/worker daemon roles have no reason to browse.
	"event emit": {roles: []string{"admin", "operator"}},
	"event list": {roles: []string{"admin", "user", "support", "operator"}},
	// User management is CREATEROLE work (role attributes are not inherited
	// through group membership), so it keys on the attribute, not a role;
	// user list and whoami only read pg_catalog, open to everyone.
	"user add":      {createRole: true},
	"user remove":   {createRole: true},
	"user set-role": {createRole: true},
	"user list":     {},
	"whoami":        {},
	// Daemon control: admin plus the dedicated operator role (0013) — an
	// operator-started daemon connects as that person's login, so
	// service_events attributes it to them. Support lost daemon access when
	// 0012 revoked 0007's service-group membership. Status is a local
	// process check, open to everyone.
	"daemon status": {},
	"daemon start":  {roles: []string{"admin", "operator"}},
	"daemon stop":   {roles: []string{"admin", "operator"}},
}

// allowedFor reports whether u can run the shell command at path, and — when
// it cannot — what access the command requires, phrased for the error/help
// text ("the admin role", "a CREATEROLE user-admin login").
func allowedFor(u models.DBUser, path string) (allowed bool, requires string) {
	p, ok := cmdPerms[path]
	if !ok {
		return true, ""
	}
	if u.Superuser {
		return true, "" // bypasses GRANTs (only reachable with a piped shell)
	}
	if p.createRole {
		if u.CreateRole {
			return true, ""
		}
		return false, "a CREATEROLE user-admin login"
	}
	if len(p.roles) == 0 {
		return true, ""
	}
	for _, g := range u.Roles {
		cli := db.CLIRoleFor(g)
		for _, r := range p.roles {
			if cli == r {
				return true, ""
			}
		}
	}
	return false, "the " + orList(p.roles) + " role"
}

// authorizeShellCmd refuses a shell command the connected role cannot run, so
// the user gets an immediate, friendly message instead of a Postgres 42501
// after the round-trip. The database still enforces access regardless.
func authorizeShellCmd(cmd *cobra.Command) error {
	path := shellPath(cmd)
	if _, ok := cmdPerms[path]; !ok {
		return nil // parents, help, and unmapped commands pass through
	}
	u, err := currentUser(cmd.Context())
	if err != nil {
		return err
	}
	if ok, requires := allowedFor(u, path); !ok {
		return fmt.Errorf("not authorized: role %q cannot run %q — this needs %s (see `whoami`)",
			u.Username, path, requires)
	}
	return nil
}

// shellPath is the command path as typed in the shell, without the binary name.
func shellPath(cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), "kronwrk ")
}

// orList joins role names for display: "admin", "admin or support",
// "admin, user or support".
func orList(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}
