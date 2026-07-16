package db

import (
	"regexp"
	"strings"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	valid := []string{"alice", "bob_2", "_ops", "a", strings.Repeat("a", 63)}
	for _, name := range valid {
		if err := ValidateUsername(name); err != nil {
			t.Errorf("ValidateUsername(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"Alice",                 // uppercase
		"1alice",                // leading digit
		"alice smith",           // space
		"alice;drop",            // punctuation
		`alice"`,                // quote
		"alice'",                // quote
		"pg_admin",              // reserved prefix
		"kronwrk_admin",         // reserved prefix
		"admin",                 // reserved role name
		"user",                  // reserved role name
		"support",               // reserved role name
		strings.Repeat("a", 64), // too long
	}
	for _, name := range invalid {
		if err := ValidateUsername(name); err == nil {
			t.Errorf("ValidateUsername(%q) = nil, want error", name)
		}
	}
}

func TestQuoteLiteral(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"plain", "'plain'"},
		{"", "''"},
		{"it's", "'it''s'"},
		{"''", "''''''"},
		{`back\slash`, `'back\slash'`}, // literal under standard_conforming_strings
	}
	for _, tt := range tests {
		got, err := quoteLiteral(tt.in)
		if err != nil {
			t.Errorf("quoteLiteral(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("quoteLiteral(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}

	if _, err := quoteLiteral("nul\x00byte"); err == nil {
		t.Error("quoteLiteral with NUL byte: want error, got nil")
	}
}

func TestGroupRoleFor(t *testing.T) {
	tests := map[string]string{
		"admin":   "kronwrk_admin",
		"user":    "kronwrk_user",
		"support": "kronwrk_support",
	}
	for cli, want := range tests {
		got, err := groupRoleFor(cli)
		if err != nil || got != want {
			t.Errorf("groupRoleFor(%q) = %q, %v; want %q, nil", cli, got, err, want)
		}
	}
	if _, err := groupRoleFor("root"); err == nil {
		t.Error(`groupRoleFor("root") = nil error, want error`)
	}
}

func TestBuildCreateUserSQL(t *testing.T) {
	password := "s3cret's"
	got, err := buildCreateUserSQL("alice", password)
	if err != nil {
		t.Fatalf("buildCreateUserSQL error: %v", err)
	}
	if !strings.HasPrefix(got, `CREATE ROLE "alice" LOGIN PASSWORD 'SCRAM-SHA-256$4096:`) ||
		!strings.HasSuffix(got, `' NOSUPERUSER NOCREATEDB NOCREATEROLE`) {
		t.Errorf("buildCreateUserSQL = %s, want SCRAM-verifier PASSWORD clause", got)
	}
	// The whole point: the plaintext must not appear in the statement text,
	// which can land in server logs under log_statement='ddl'.
	if strings.Contains(got, password) {
		t.Errorf("buildCreateUserSQL leaks the plaintext password: %s", got)
	}

	if _, err := buildCreateUserSQL("bad name", "pw"); err == nil {
		t.Error("buildCreateUserSQL with invalid username: want error, got nil")
	}
	if _, err := buildCreateUserSQL("alice", "nul\x00"); err == nil {
		t.Error("buildCreateUserSQL with NUL in password: want error, got nil")
	}
}

func TestBuildSCRAMVerifier(t *testing.T) {
	// Format per RFC 7677 as stored in pg_authid:
	// SCRAM-SHA-256$<iter>:<b64 salt>$<b64 StoredKey>:<b64 ServerKey>
	verifierRE := regexp.MustCompile(`^SCRAM-SHA-256\$4096:[A-Za-z0-9+/=]+\$[A-Za-z0-9+/=]+:[A-Za-z0-9+/=]+$`)
	v1, err := buildSCRAMVerifier("hunter2")
	if err != nil {
		t.Fatalf("buildSCRAMVerifier error: %v", err)
	}
	if !verifierRE.MatchString(v1) {
		t.Errorf("verifier %q does not match the SCRAM-SHA-256 format", v1)
	}
	// Random salt: same password twice must not produce the same verifier.
	if v2, _ := buildSCRAMVerifier("hunter2"); v1 == v2 {
		t.Error("two verifiers for the same password are identical (salt not random)")
	}
}

func TestCLIRoleFor(t *testing.T) {
	if got := CLIRoleFor("kronwrk_support"); got != "support" {
		t.Errorf(`CLIRoleFor("kronwrk_support") = %q, want "support"`, got)
	}
	// Unknown groups pass through for display rather than being hidden.
	if got := CLIRoleFor("other_role"); got != "other_role" {
		t.Errorf(`CLIRoleFor("other_role") = %q, want "other_role"`, got)
	}
}
