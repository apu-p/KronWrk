package main

import (
	"testing"

	"kronwrk/internal/models"
)

func TestParseWaitFlag(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"90m", 5400, false},
		{"1h30m", 5400, false},
		{"45s", 45, false},
		{"500ms", 0, true},
		{"-1h", 0, true},
		{"soon", 0, true},
	}
	for _, tt := range tests {
		got, err := parseWaitFlag(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseWaitFlag(%q) error = %v, wantErr %t", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("parseWaitFlag(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestEventsTableViewDimsConsumed(t *testing.T) {
	run := int64(12)
	events := []models.Event{
		{ID: 2, Name: "etl_done", EmittedBy: "alice"},
		{ID: 1, Name: "etl_done", EmittedBy: "alice", ConsumedByRunID: &run},
	}
	v := eventsTableView(events)
	if want := []bool{false, true}; v.dim[0] != want[0] || v.dim[1] != want[1] {
		t.Errorf("dim = %v, want %v (consumed events shaded)", v.dim, want)
	}
	if got := v.rows[1][4]; got != "12" {
		t.Errorf("consumed-by cell = %q, want %q", got, "12")
	}
	if got := v.rows[0][4]; got != "-" {
		t.Errorf("unconsumed consumed-by cell = %q, want %q", got, "-")
	}
}
