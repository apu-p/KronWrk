package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitArgs(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"job list", []string{"job", "list"}},
		{"  job   list  ", []string{"job", "list"}},
		{`job add --name nightly --schedule "0 2 * * *"`, []string{"job", "add", "--name", "nightly", "--schedule", "0 2 * * *"}},
		{`job add --args 'a b,c d'`, []string{"job", "add", "--args", "a b,c d"}},
		{`echo it\'s`, []string{"echo", "it's"}},
		{`echo "she said \"hi\""`, []string{"echo", `she said "hi"`}},
		{`echo ''`, []string{"echo", ""}},
		{"", nil},
	}
	for _, tt := range tests {
		got, err := splitArgs(tt.in)
		if err != nil {
			t.Errorf("splitArgs(%q) error: %v", tt.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitArgs(%q) = %#v, want %#v", tt.in, got, tt.want)
		}
	}

	for _, bad := range []string{`echo "unclosed`, `echo 'unclosed`, `echo trailing\`} {
		if _, err := splitArgs(bad); err == nil {
			t.Errorf("splitArgs(%q) = nil error, want error", bad)
		}
	}
}

func TestSplitCSVArgs(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ", []string{"a", "b"}},
		{"a,,b", []string{"a", "b"}},
		{"", nil},
		{" , ", nil},
	}
	for _, tt := range tests {
		if got := splitCSVArgs(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitCSVArgs(%q) = %#v, want %#v", tt.in, got, tt.want)
		}
	}
}

func TestNextRunsPreview(t *testing.T) {
	got := nextRunsPreview("*/5 * * * *", "UTC", 3)
	if lines := strings.Split(got, "\n"); len(lines) != 3 {
		t.Errorf("nextRunsPreview valid expr: got %d lines, want 3:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(got, "next: ") {
		t.Errorf("nextRunsPreview valid expr: want 'next: ' prefix, got %q", got)
	}

	if got := nextRunsPreview("not a cron", "UTC", 3); !strings.HasPrefix(got, "invalid: ") {
		t.Errorf("nextRunsPreview invalid expr: want 'invalid: ' prefix, got %q", got)
	}

	if got := nextRunsPreview("", "UTC", 3); !strings.Contains(got, "5-field cron") {
		t.Errorf("nextRunsPreview empty expr: want usage hint, got %q", got)
	}
}
