package main

import (
	"strings"
	"testing"

	"kronwrk/internal/models"
)

// userWith builds a DBUser holding the given kronwrk_* groups.
func userWith(groups ...string) models.DBUser {
	return models.DBUser{Username: "test", Roles: groups}
}

func TestAllowedFor(t *testing.T) {
	operator := models.DBUser{Username: "alice", Roles: []string{"kronwrk_admin"}, CreateRole: true}
	tests := []struct {
		name string
		u    models.DBUser
		path string
		want bool
	}{
		{"admin adds jobs", userWith("kronwrk_admin"), "job add", true},
		{"user cannot add jobs", userWith("kronwrk_user"), "job add", false},
		{"user lists jobs", userWith("kronwrk_user"), "job list", true},
		{"user cannot disable", userWith("kronwrk_user"), "job disable", false},
		{"support disables jobs", userWith("kronwrk_support"), "job disable", true},
		{"support cannot add jobs", userWith("kronwrk_support"), "job add", false},
		{"scheduler cannot read runs", userWith("kronwrk_scheduler"), "run status", false},
		{"user can monitor", userWith("kronwrk_user"), "monitor", true},
		{"scheduler cannot monitor", userWith("kronwrk_scheduler"), "monitor", false},
		{"worker reads runs", userWith("kronwrk_worker"), "run status", true},
		{"worker cannot disable", userWith("kronwrk_worker"), "job disable", false},
		{"plain admin cannot manage users", userWith("kronwrk_admin"), "user add", false},
		{"operator manages users", operator, "user set-role", true},
		{"roleless login runs whoami", userWith(), "whoami", true},
		{"roleless login lists users", userWith(), "user list", true},
		{"roleless login cannot list jobs", userWith(), "job list", false},
		{"admin starts daemons", userWith("kronwrk_admin"), "daemon start", true},
		{"operator starts daemons", userWith("kronwrk_operator"), "daemon start", true},
		{"operator stops daemons", userWith("kronwrk_operator"), "daemon stop", true},
		{"operator monitors", userWith("kronwrk_operator"), "monitor", true},
		{"operator cannot add jobs", userWith("kronwrk_operator"), "job add", false},
		{"operator cannot manage users", userWith("kronwrk_operator"), "user add", false},
		{"support cannot stop daemons (0012)", userWith("kronwrk_support"), "daemon stop", false},
		{"user cannot start daemons", userWith("kronwrk_user"), "daemon start", false},
		{"worker role cannot stop daemons", userWith("kronwrk_worker"), "daemon stop", false},
		{"roleless login sees daemon status", userWith(), "daemon status", true},
		{"operator emits events", userWith("kronwrk_operator"), "event emit", true},
		{"admin emits events", userWith("kronwrk_admin"), "event emit", true},
		{"user cannot emit events", userWith("kronwrk_user"), "event emit", false},
		{"support cannot emit events", userWith("kronwrk_support"), "event emit", false},
		{"user lists events", userWith("kronwrk_user"), "event list", true},
		{"scheduler cannot list events", userWith("kronwrk_scheduler"), "event list", false},
		{"admin adds conditions", userWith("kronwrk_admin"), "job condition add", true},
		{"support cannot add conditions", userWith("kronwrk_support"), "job condition add", false},
		{"operator cannot remove conditions", userWith("kronwrk_operator"), "job condition remove", false},
		{"scheduler lists conditions", userWith("kronwrk_scheduler"), "job condition list", true},
		{"unmapped command passes through", userWith(), "job", true},
		{"superuser bypasses grants", models.DBUser{Username: "root", Superuser: true}, "job add", true},
		{"drifted multi-role uses any match", userWith("kronwrk_user", "kronwrk_support"), "job enable", true},
	}
	for _, tt := range tests {
		if got, _ := allowedFor(tt.u, tt.path); got != tt.want {
			t.Errorf("%s: allowedFor(%v, %q) = %t, want %t", tt.name, tt.u.Roles, tt.path, got, tt.want)
		}
	}
}

func TestAllowedForRequiresText(t *testing.T) {
	if _, req := allowedFor(userWith("kronwrk_user"), "job add"); req != "the admin role" {
		t.Errorf("job add requires = %q, want %q", req, "the admin role")
	}
	if _, req := allowedFor(userWith("kronwrk_admin"), "user add"); req != "a CREATEROLE user-admin login" {
		t.Errorf("user add requires = %q, want %q", req, "a CREATEROLE user-admin login")
	}
	if _, req := allowedFor(userWith("kronwrk_user"), "job disable"); !strings.Contains(req, " or ") {
		t.Errorf("job disable requires = %q, want an or-joined role list", req)
	}
}

// TestCmdPermsMatchCommandTree ensures every mapped path resolves to a real
// command, so the matrix cannot silently drift from the Cobra tree.
func TestCmdPermsMatchCommandTree(t *testing.T) {
	root := rootCmd()
	for path := range cmdPerms {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil || shellPath(cmd) != path {
			t.Errorf("cmdPerms entry %q does not match a command in the tree", path)
		}
	}
	// And every shell-visible leaf is mapped, so new commands get a deliberate
	// entry (or a deliberate omission fails here first).
	for _, c := range root.Commands() {
		if c.Hidden || !requiresShell(c) {
			continue
		}
		for _, leaf := range shellLeaves(c) {
			if _, ok := cmdPerms[shellPath(leaf)]; !ok {
				t.Errorf("shell command %q has no cmdPerms entry", shellPath(leaf))
			}
		}
	}
}
