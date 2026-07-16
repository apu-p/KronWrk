package worker

import (
	"strings"
	"testing"
)

// TestJobEnvExcludesSecrets is a security guard: job commands must never
// inherit the worker's DATABASE_URL (or other kronwrk config), otherwise any
// job could read the daemon's database credential out of its environment.
func TestJobEnvExcludesSecrets(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:secret@localhost:5432/kronwrk")
	t.Setenv("POLL_INTERVAL", "5s")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("LC_TESTVAR", "en_US.UTF-8")

	env := jobEnv()

	for _, kv := range env {
		key := kv[:strings.IndexByte(kv, '=')]
		switch key {
		case "DATABASE_URL", "POLL_INTERVAL", "WORKER_CONCURRENCY",
			"HEARTBEAT_INTERVAL", "JOB_TIMEOUT_DEFAULT", "LOG_LEVEL":
			t.Errorf("jobEnv leaked kronwrk config %q to the job environment", key)
		}
	}

	if !hasKey(env, "PATH") {
		t.Error("jobEnv should pass through PATH")
	}
	if !hasKey(env, "LC_TESTVAR") {
		t.Error("jobEnv should pass through LC_* locale vars")
	}
}

func hasKey(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}
