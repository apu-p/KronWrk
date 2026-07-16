package db

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/text/secure/precis"

	"kronwrk/internal/models"
)

// GroupRoles maps CLI role names to the Postgres group roles created by
// migrations 0003 (admin/user/support, renamed kronwrk_* in 0004), 0006
// (scheduler/worker daemon roles), and 0013 (operator: on-duty staff who
// start/stop daemons under their own login for audit attribution). Access is
// enforced by the database via these roles' GRANTs; the app never gates
// commands itself.
var GroupRoles = map[string]string{
	"admin":     "kronwrk_admin",
	"user":      "kronwrk_user",
	"support":   "kronwrk_support",
	"scheduler": "kronwrk_scheduler",
	"worker":    "kronwrk_worker",
	"operator":  "kronwrk_operator",
}

// RoleNames returns the CLI role names, sorted, for help text and validation
// messages.
func RoleNames() []string {
	names := make([]string, 0, len(GroupRoles))
	for name := range GroupRoles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// groupRoleFor resolves a CLI role name to its Postgres group role.
func groupRoleFor(cliRole string) (string, error) {
	group, ok := GroupRoles[cliRole]
	if !ok {
		return "", fmt.Errorf("unknown role %q (valid roles: %s)", cliRole, strings.Join(RoleNames(), ", "))
	}
	return group, nil
}

// groupRoleList is the comma-separated list of every kronwrk_* group role, used
// in the revoke-all step that enforces one-role-per-user.
func groupRoleList() string {
	return strings.Join(groupRoleValues(), ", ")
}

// usernamePattern is a deliberately safe subset of what Postgres allows in an
// identifier: lowercase start, then lowercase/digits/underscore, max 63 bytes
// (NAMEDATALEN-1).
var usernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// ValidateUsername rejects names that are not safe, simple identifiers, that
// collide with reserved prefixes (pg_ is reserved by Postgres; kronwrk_ is
// reserved for the group roles), or that match a role name (admin/user/support/
// scheduler/worker) — a user named after its own role is ambiguous, "admin"
// also collides with the operator login role, and reserving "scheduler"/"worker"
// keeps a daemon login from shadowing its own group role.
func ValidateUsername(name string) error {
	if !usernamePattern.MatchString(name) {
		return fmt.Errorf("invalid username %q: must match %s", name, usernamePattern.String())
	}
	if strings.HasPrefix(name, "pg_") || strings.HasPrefix(name, "kronwrk_") {
		return fmt.Errorf("invalid username %q: the pg_ and kronwrk_ prefixes are reserved", name)
	}
	if _, isRole := GroupRoles[name]; isRole {
		return fmt.Errorf("invalid username %q: reserved — it is a role name (%s)", name, strings.Join(RoleNames(), ", "))
	}
	return nil
}

// quoteLiteral escapes a string for embedding as a SQL string literal. Needed
// because CREATE/ALTER ROLE are DDL and cannot take bind parameters. Doubling
// single quotes is sufficient under standard_conforming_strings (always on
// since PostgreSQL 9.1); NUL bytes cannot be represented and are rejected.
func quoteLiteral(s string) (string, error) {
	if strings.ContainsRune(s, 0) {
		return "", fmt.Errorf("string literal must not contain NUL bytes")
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'", nil
}

// scramIterations is the PBKDF2 iteration count for generated verifiers —
// PostgreSQL's own default for scram-sha-256.
const scramIterations = 4096

// buildSCRAMVerifier hashes a password into a SCRAM-SHA-256 verifier — the
// exact string the server would store — so role DDL never contains the
// plaintext: CREATE/ALTER ROLE statement text lands in server logs under
// log_statement='ddl'/'all', and a verifier is safe to log where a password
// is not. Preparation mirrors the pgx client (RFC 8265 OpaqueString, falling
// back to the raw password when preparation rejects it) so the verifier and
// future logins derive from the same bytes.
func buildSCRAMVerifier(password string) (string, error) {
	if strings.ContainsRune(password, 0) {
		return "", fmt.Errorf("password must not contain NUL bytes")
	}
	prepared, err := precis.OpaqueString.String(password)
	if err != nil {
		prepared = password
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	salted := pbkdf2.Key([]byte(prepared), salt, scramIterations, sha256.Size, sha256.New)
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")

	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		scramIterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// buildCreateUserSQL returns the CREATE ROLE statement for a new login user,
// with the password pre-hashed to a SCRAM verifier (see buildSCRAMVerifier).
// Pure function so the quoting logic is unit-testable without a database.
func buildCreateUserSQL(username, password string) (string, error) {
	if err := ValidateUsername(username); err != nil {
		return "", err
	}
	verifier, err := buildSCRAMVerifier(password)
	if err != nil {
		return "", fmt.Errorf("invalid password: %w", err)
	}
	quoted, err := quoteLiteral(verifier)
	if err != nil {
		return "", fmt.Errorf("invalid password: %w", err)
	}
	return fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD %s NOSUPERUSER NOCREATEDB NOCREATEROLE",
		pgx.Identifier{username}.Sanitize(), quoted), nil
}

// CreateUser creates a LOGIN role and grants it exactly one kronwrk_* group,
// in one transaction so a failed GRANT can't leave a groupless login behind.
// The connected role needs CREATEROLE plus ADMIN OPTION on the group role — in
// practice the bootstrap superuser DATABASE_URL.
func (s *Store) CreateUser(ctx context.Context, username, password, cliRole string) error {
	group, err := groupRoleFor(cliRole)
	if err != nil {
		return err
	}
	createSQL, err := buildCreateUserSQL(username, password)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, createSQL); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("GRANT %s TO %s", group, pgx.Identifier{username}.Sanitize())); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetUserRole reassigns a user to a different role. Revoking all three groups
// before granting the new one is what enforces one-role-per-user.
func (s *Store) SetUserRole(ctx context.Context, username, cliRole string) error {
	group, err := groupRoleFor(cliRole)
	if err != nil {
		return err
	}
	if err := ValidateUsername(username); err != nil {
		return err
	}
	ident := pgx.Identifier{username}.Sanitize()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, fmt.Sprintf("REVOKE %s FROM %s", groupRoleList(), ident)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("GRANT %s TO %s", group, ident)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DropUser removes a login role created by CreateUser.
func (s *Store) DropUser(ctx context.Context, username string) error {
	if err := ValidateUsername(username); err != nil {
		return err
	}
	ident := pgx.Identifier{username}.Sanitize()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, fmt.Sprintf("REVOKE %s FROM %s", groupRoleList(), ident)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("DROP ROLE %s", ident)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListUsers returns every login role that is a member of a kronwrk_* group.
func (s *Store) ListUsers(ctx context.Context) ([]models.DBUser, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.rolname, g.rolname
		FROM pg_auth_members am
		JOIN pg_roles m ON m.oid = am.member
		JOIN pg_roles g ON g.oid = am.roleid
		WHERE g.rolname = ANY($1) AND am.inherit_option
		ORDER BY m.rolname, g.rolname`,
		groupRoleValues())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []models.DBUser
	for rows.Next() {
		var member, group string
		if err := rows.Scan(&member, &group); err != nil {
			return nil, err
		}
		if n := len(users); n > 0 && users[n-1].Username == member {
			users[n-1].Roles = append(users[n-1].Roles, group)
		} else {
			users = append(users, models.DBUser{Username: member, Roles: []string{group}})
		}
	}
	return users, rows.Err()
}

// WhoAmI reports the connected role and the kronwrk_* groups whose privileges
// it actually inherits. Direct membership (pg_auth_members) rather than
// pg_has_role, because a superuser passes pg_has_role for every role and would
// list all three. The `am.inherit_option` filter excludes admin-option-only
// grants — the operator login holds ADMIN OPTION on every group (to grant/revoke
// them for user management) but only inherits kronwrk_admin, so the extra grants
// must not surface it as a multi-role user.
func (s *Store) WhoAmI(ctx context.Context) (models.DBUser, error) {
	u := models.DBUser{}
	err := s.pool.QueryRow(ctx, `
		SELECT current_user,
		       r.rolsuper,
		       r.rolcreaterole,
		       COALESCE(array_agg(g.rolname ORDER BY g.rolname)
		                FILTER (WHERE g.rolname IS NOT NULL), '{}')
		FROM pg_roles r
		LEFT JOIN pg_auth_members am ON am.member = r.oid AND am.inherit_option
		LEFT JOIN pg_roles g ON g.oid = am.roleid AND g.rolname = ANY($1)
		WHERE r.rolname = current_user
		GROUP BY r.rolname, r.rolsuper, r.rolcreaterole`,
		groupRoleValues()).Scan(&u.Username, &u.Superuser, &u.CreateRole, &u.Roles)
	return u, err
}

// groupRoleValues returns the group role names as a slice for ANY($1) binds.
func groupRoleValues() []string {
	groups := make([]string, 0, len(GroupRoles))
	for _, g := range GroupRoles {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	return groups
}

// CLIRoleFor maps a Postgres group role back to its CLI name for display.
func CLIRoleFor(group string) string {
	for cli, g := range GroupRoles {
		if g == group {
			return cli
		}
	}
	return group
}
