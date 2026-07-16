package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"kronwrk/internal/models"
)

func TestMonitorTableView(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	next := now.Add(30 * time.Minute)
	last := now.Add(-2 * time.Hour)
	status := "succeeded"
	overviews := []models.JobOverview{
		{ID: 4, Name: "nightly", ScheduleExpr: "0 2 * * *", Timezone: "UTC", Enabled: true,
			NextRunAt: &next, LastRunAt: &last, LastStatus: &status},
		{ID: 7, Name: "new-job", ScheduleExpr: "*/5 * * * *", Timezone: "UTC", Enabled: false},
	}

	v := monitorTableView(overviews, now)
	wantCols := []string{"ID", "NAME", "SCHEDULE", "TIMEZONE", "ENABLED", "NEXT RUN", "LAST RUN", "STATUS"}
	if !reflect.DeepEqual(v.columns, wantCols) {
		t.Errorf("columns = %v, want %v", v.columns, wantCols)
	}
	wantRows := [][]string{
		{"4", "nightly", "0 2 * * *", "UTC", "yes", "2026-07-05 12:30 UTC (in 30m)", "2026-07-05 10:00 UTC", "succeeded"},
		{"7", "new-job", "*/5 * * * *", "UTC", "no", "-", "-", "-"},
	}
	if !reflect.DeepEqual(v.rows, wantRows) {
		t.Errorf("rows = %v, want %v", v.rows, wantRows)
	}
	if want := []bool{false, true}; !reflect.DeepEqual(v.dim, want) {
		t.Errorf("dim = %v, want %v", v.dim, want)
	}
}

func TestMonitorColumnsWidths(t *testing.T) {
	v := tableView{
		columns: []string{"ID", "NAME"},
		rows:    [][]string{{"1", "a-name-longer-than-the-header"}},
	}
	cols := monitorColumns(v)
	if cols[0].Width != 2 {
		t.Errorf("ID width = %d, want header width 2", cols[0].Width)
	}
	if want := len("a-name-longer-than-the-header"); cols[1].Width != want {
		t.Errorf("NAME width = %d, want %d", cols[1].Width, want)
	}
}

func TestRunsTableView(t *testing.T) {
	started := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	finished := started.Add(90 * time.Second)
	code := 0
	worker := "host-42"
	longErr := strings.Repeat("e", 45)
	runs := []models.JobRun{
		// In-flight: no finished_at, no exit code yet.
		{ID: 9, Status: "running", ScheduledFor: started, StartedAt: &started, WorkerID: &worker},
		{ID: 8, Status: "succeeded", ScheduledFor: started, StartedAt: &started, FinishedAt: &finished,
			ExitCode: &code, WorkerID: &worker},
		{ID: 7, Status: "failed", ScheduledFor: started, ErrorMessage: &longErr},
		{ID: 6, Status: "skipped", ScheduledFor: started, FinishedAt: &finished},
	}

	v := runsTableView(runs, "UTC")
	wantCols := []string{"RUN", "STATUS", "SCHEDULED FOR", "STARTED", "FINISHED", "EXIT", "WORKER", "ERROR"}
	if !reflect.DeepEqual(v.columns, wantCols) {
		t.Errorf("columns = %v, want %v", v.columns, wantCols)
	}
	if got := v.rows[0]; got[4] != "-" || got[5] != "-" {
		t.Errorf("running row should show '-' for finished/exit, got %v", got)
	}
	if got := v.rows[1]; got[4] != "2026-07-05 10:01 UTC" || got[5] != "0" {
		t.Errorf("finished row = %v, want finished ts and exit 0", got)
	}
	if got := v.rows[2][7]; len([]rune(got)) != 30 {
		t.Errorf("error cell should truncate to 30 runes, got %d (%q)", len([]rune(got)), got)
	}
	if want := []bool{false, false, false, true}; !reflect.DeepEqual(v.dim, want) {
		t.Errorf("dim = %v, want %v (only the skipped run shaded)", v.dim, want)
	}
}

func TestFitColumnsStretch(t *testing.T) {
	v := tableView{
		columns: []string{"ID", "NAME", "STATUS"},
		rows:    [][]string{{"1", "short", "queued"}},
	}
	cols := fitColumns(v, 80)
	total := 0
	for _, c := range cols {
		total += c.Width
	}
	if want := 80 - cellPadding*len(cols); total != want {
		t.Errorf("stretched widths sum = %d, want %d (fill the terminal)", total, want)
	}
	if cols[stretchColumn].Title != "NAME" || cols[stretchColumn].Width <= len("short") {
		t.Errorf("NAME should absorb the surplus, got %+v", cols[stretchColumn])
	}
}

func TestFitColumnsShrink(t *testing.T) {
	v := tableView{
		columns: []string{"ID", "NAME", "NEXT RUN"},
		rows:    [][]string{{"1", strings.Repeat("n", 40), strings.Repeat("t", 35)}},
	}
	termWidth := 40
	cols := fitColumns(v, termWidth)
	total := 0
	for _, c := range cols {
		total += c.Width
		if c.Width < 2 {
			t.Errorf("column %s shrunk below its header floor: %d", c.Title, c.Width)
		}
	}
	if budget := termWidth - cellPadding*len(cols); total > budget {
		t.Errorf("shrunk widths sum = %d, exceeds budget %d", total, budget)
	}
}
