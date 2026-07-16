package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
)

// tableView is a render-agnostic table: column headers plus preformatted
// string rows, with an optional per-row dim flag (e.g. disabled jobs).
// Commands build one of these instead of printing directly, so the static
// lipgloss rendering below and a future interactive view (bubbles/table takes
// the same columns+rows shape) share the exact same data.
type tableView struct {
	columns []string
	rows    [][]string
	dim     []bool // len(rows) when set; dim[i] renders row i in the faint shade
}

// printTable renders v in the shell's accent theme on a TTY, or as plain
// tab-separated output when stdout is piped so scripts can keep parsing it.
func printTable(v tableView) error {
	if !stdoutIsTTY() {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, strings.Join(v.columns, "\t"))
		for _, r := range v.rows {
			fmt.Fprintln(tw, strings.Join(r, "\t"))
		}
		return tw.Flush()
	}

	cell := lipgloss.NewStyle().Padding(0, 1)
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(accentColor)).
		Headers(v.columns...).
		Rows(v.rows...).
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return cell.Bold(true).Foreground(accentColor)
			}
			if row < len(v.dim) && v.dim[row] {
				return cell.Faint(true)
			}
			return cell
		})
	fmt.Println(t)
	return nil
}

// stdoutIsTTY reports whether stdout is a terminal; pretty rendering is
// reserved for humans, piped output stays machine-parseable.
func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// relTime says how far away t is, e.g. "in 25m", "overdue 3m".
func relTime(t time.Time, now time.Time) string {
	d := t.Sub(now)
	if d >= 0 {
		return "in " + shortDur(d)
	}
	return "overdue " + shortDur(-d)
}

// truncate caps a cell at n runes, marking the cut with an ellipsis, so one
// long free-text value can't blow up a table row.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// shortDur renders a duration at the coarsest useful precision: "45s", "25m",
// "3h05m", "12d".
func shortDur(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
