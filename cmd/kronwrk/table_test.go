package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"kronwrk/internal/models"
)

func TestShortDur(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{25*time.Minute + 30*time.Second, "25m"},
		{3*time.Hour + 5*time.Minute, "3h05m"},
		{47 * time.Hour, "47h00m"},
		{72 * time.Hour, "3d"},
		{0, "0s"},
	}
	for _, tt := range tests {
		if got := shortDur(tt.d); got != tt.want {
			t.Errorf("shortDur(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestRelTime(t *testing.T) {
	now := time.Now()
	if got := relTime(now.Add(10*time.Minute), now); got != "in 10m" {
		t.Errorf("relTime future = %q, want %q", got, "in 10m")
	}
	if got := relTime(now.Add(-90*time.Second), now); got != "overdue 1m" {
		t.Errorf("relTime past = %q, want %q", got, "overdue 1m")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 40); got != "short" {
		t.Errorf("truncate(short) = %q, want unchanged", got)
	}
	long := strings.Repeat("x", 45)
	if got := truncate(long, 40); len([]rune(got)) != 40 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate(long, 40) = %q, want 40 runes ending in …", got)
	}
}

func TestJobsTableView(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	next := now.Add(2 * time.Hour)
	jobs := []models.Job{
		{ID: 1, Name: "nightly", ScheduleExpr: "0 2 * * *", Timezone: "UTC", Enabled: true, NextRunAt: &next, Comment: "CHG-1234 nightly report"},
		{ID: 2, Name: "stopped", ScheduleExpr: "* * * * *", Timezone: "UTC", Enabled: false, NextRunAt: nil},
	}

	v := jobsTableView(jobs, now)
	wantCols := []string{"ID", "NAME", "SCHEDULE", "TIMEZONE", "ENABLED", "NEXT RUN", "COMMENT"}
	if !reflect.DeepEqual(v.columns, wantCols) {
		t.Errorf("columns = %v, want %v", v.columns, wantCols)
	}
	wantRows := [][]string{
		{"1", "nightly", "0 2 * * *", "UTC", "yes", "2026-07-04 14:00 UTC (in 2h00m)", "CHG-1234 nightly report"},
		{"2", "stopped", "* * * * *", "UTC", "no", "-", ""},
	}
	if !reflect.DeepEqual(v.rows, wantRows) {
		t.Errorf("rows = %v, want %v", v.rows, wantRows)
	}
	if want := []bool{false, true}; !reflect.DeepEqual(v.dim, want) {
		t.Errorf("dim = %v, want %v (disabled jobs render faint)", v.dim, want)
	}
}
