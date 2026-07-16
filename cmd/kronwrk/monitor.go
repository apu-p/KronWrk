package main

import (
	"context"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"kronwrk/internal/db"
	"kronwrk/internal/models"
)

// monitor is the interactive jobs overview: every job joined with its latest
// run, in a navigable bubbles/table (arrow keys move the highlighted cursor
// row, F5 re-queries, q/Esc leaves). It consumes the same tableView shape the
// static tables use, so the data layer is shared with a plain print fallback
// for piped/scripted invocations.

func monitorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "monitor",
		Short: "Interactive overview of jobs (↑/↓ move, Enter run history, F5 refresh, q quit)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			store, err := getStore(ctx)
			if err != nil {
				return err
			}
			overviews, err := store.ListJobOverviews(ctx)
			if err != nil {
				return err
			}
			view := monitorTableView(overviews, time.Now())
			if !stdinIsTTY() {
				return printTable(view) // scripted use gets a one-shot table
			}
			m := newMonitorModel(ctx, store, view)
			_, err = tea.NewProgram(m, tea.WithAltScreen()).Run()
			return err
		},
	}
}

// monitorTableView shapes job overviews for display; pure (now is a
// parameter) and shared by the interactive table and the piped fallback.
func monitorTableView(overviews []models.JobOverview, now time.Time) tableView {
	v := tableView{columns: []string{"ID", "NAME", "SCHEDULE", "TIMEZONE", "ENABLED", "NEXT RUN", "LAST RUN", "STATUS"}}
	for _, o := range overviews {
		nextRun := "-"
		if o.NextRunAt != nil {
			nextRun = fmt.Sprintf("%s (%s)", fmtZonedTime(o.NextRunAt, o.Timezone), relTime(*o.NextRunAt, now))
		}
		v.rows = append(v.rows, []string{
			strconv.FormatInt(o.ID, 10),
			o.Name,
			o.ScheduleExpr,
			o.Timezone,
			map[bool]string{true: "yes", false: "no"}[o.Enabled],
			nextRun,
			fmtZonedTime(o.LastRunAt, o.Timezone),
			deref(o.LastStatus),
		})
		v.dim = append(v.dim, !o.Enabled)
	}
	return v
}

// fmtZonedTime renders a timestamp in the job's own timezone, "-" when nil.
func fmtZonedTime(t *time.Time, tz string) string {
	if t == nil {
		return "-"
	}
	tt := *t
	if loc, err := time.LoadLocation(tz); err == nil {
		tt = tt.In(loc)
	}
	return tt.Format("2006-01-02 15:04 MST")
}

// --- bubbletea model ---

// monitorDataMsg carries a completed jobs-overview refresh back into the
// update loop.
type monitorDataMsg struct {
	view tableView
	err  error
}

// runsDataMsg carries a completed run-history query for the drill-down view.
type runsDataMsg struct {
	view tableView
	err  error
}

// monitorMode selects which of the two screens is active.
type monitorMode int

const (
	jobsMode monitorMode = iota // the jobs overview table
	runsMode                    // one job's run history (Enter on a row)
)

type monitorModel struct {
	ctx         context.Context
	store       *db.Store
	mode        monitorMode
	tbl         table.Model // jobs overview; keeps its cursor while drilled in
	view        tableView
	runsTbl     table.Model
	runsView    tableView
	runsReady   bool // runsTbl has been through newStyledTable
	selJobID    int64
	selJobName  string
	selJobTZ    string
	width       int // terminal size from the latest tea.WindowSizeMsg
	height      int
	refreshedAt time.Time
	err         error
}

func newMonitorModel(ctx context.Context, store *db.Store, view tableView) monitorModel {
	return monitorModel{
		ctx:         ctx,
		store:       store,
		tbl:         newStyledTable(view),
		view:        view,
		refreshedAt: time.Now(),
	}
}

// newStyledTable builds a focused bubbles table in the shell's accent theme.
// Initial sizing is provisional: bubbletea delivers a WindowSizeMsg before
// the first paint, and relayout takes over from there.
func newStyledTable(view tableView) table.Model {
	tbl := table.New(
		table.WithColumns(monitorColumns(view)),
		table.WithRows(monitorRows(view)),
		table.WithHeight(clamp(len(view.rows), 3, 20)),
		table.WithFocused(true),
	)
	st := table.DefaultStyles()
	st.Header = st.Header.
		Bold(true).
		Foreground(accentColor).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(accentColor).
		BorderBottom(true)
	st.Selected = st.Selected.
		Bold(true).
		Foreground(lipgloss.Color("230")).
		Background(accentColor)
	tbl.SetStyles(st)
	return tbl
}

// chromeLines is everything View renders around the table's row viewport:
// title, two blank separators, the footer, one spare line for a refresh
// error, plus the table's own header row and its bottom border.
const chromeLines = 7

// relayout resizes both tables to the terminal: full-width columns and a row
// viewport that fills the remaining height. Called on every WindowSizeMsg
// (terminal changed) and data message (content widths changed).
func (m *monitorModel) relayout() {
	if m.width == 0 {
		return // no WindowSizeMsg yet; keep the provisional layout
	}
	h := clamp(m.height-chromeLines, 3, m.height)
	m.tbl.SetColumns(fitColumns(m.view, m.width))
	m.tbl.SetHeight(h)
	if m.runsReady {
		m.runsTbl.SetColumns(fitColumns(m.runsView, m.width))
		m.runsTbl.SetHeight(h)
	}
}

func (m monitorModel) Init() tea.Cmd { return nil }

func (m monitorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.relayout()
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc", "backspace":
			if m.mode == runsMode {
				m.mode, m.err = jobsMode, nil // back to the overview, cursor intact
				return m, nil
			}
			if msg.String() == "esc" {
				return m, tea.Quit
			}
		case "f5", "r":
			if m.mode == runsMode {
				return m, m.refreshRuns(m.selJobID, m.selJobTZ)
			}
			return m, m.refresh()
		case "enter":
			if m.mode == jobsMode {
				if row := m.tbl.SelectedRow(); row != nil {
					if id, err := strconv.ParseInt(row[0], 10, 64); err == nil {
						m.selJobID, m.selJobName, m.selJobTZ = id, row[1], row[3]
						return m, m.refreshRuns(id, row[3])
					}
				}
			}
		}
	case monitorDataMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.refreshedAt = time.Now()
		m.view = msg.view
		m.tbl.SetRows(monitorRows(msg.view))
		m.relayout()
		return m, nil
	case runsDataMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.refreshedAt = time.Now()
		m.runsView = msg.view
		if m.runsReady {
			m.runsTbl.SetRows(monitorRows(msg.view)) // refresh in place, keep cursor
		} else {
			m.runsTbl = newStyledTable(msg.view)
			m.runsReady = true
		}
		m.mode = runsMode
		m.relayout()
		return m, nil
	}
	var cmd tea.Cmd
	if m.mode == runsMode {
		m.runsTbl, cmd = m.runsTbl.Update(msg)
	} else {
		m.tbl, cmd = m.tbl.Update(msg)
	}
	return m, cmd
}

func (m monitorModel) View() string {
	var s string
	if m.mode == runsMode {
		s = accentText("Run history") +
			faintText(fmt.Sprintf("  job %d (%s) · in-flight first, then newest finished", m.selJobID, m.selJobName)) + "\n\n"
		s += m.runsTbl.View() + "\n"
		if len(m.runsView.rows) == 0 {
			s += faintText("  (no runs recorded for this job yet)") + "\n"
		}
		s += "\n" + faintText(fmt.Sprintf("  ↑/↓ move · Esc back · F5 refresh · q quit · refreshed %s", m.refreshedAt.Format("15:04:05")))
	} else {
		s = accentText("Monitor") + faintText("  jobs and their most recent run") + "\n\n"
		s += m.tbl.View() + "\n\n"
		s += faintText(fmt.Sprintf("  ↑/↓ move · Enter run history · F5 refresh · q quit · refreshed %s", m.refreshedAt.Format("15:04:05")))
	}
	if m.err != nil {
		s += "\n" + faintText("  refresh failed: ") + m.err.Error()
	}
	return s
}

// refresh re-queries the overview off the update loop and reports back as a
// monitorDataMsg.
func (m monitorModel) refresh() tea.Cmd {
	return func() tea.Msg {
		overviews, err := m.store.ListJobOverviews(m.ctx)
		if err != nil {
			return monitorDataMsg{err: err}
		}
		return monitorDataMsg{view: monitorTableView(overviews, time.Now())}
	}
}

// refreshRuns queries one job's run history off the update loop and reports
// back as a runsDataMsg.
func (m monitorModel) refreshRuns(jobID int64, tz string) tea.Cmd {
	return func() tea.Msg {
		runs, err := m.store.ListRunsForJob(m.ctx, jobID)
		if err != nil {
			return runsDataMsg{err: err}
		}
		return runsDataMsg{view: runsTableView(runs, tz)}
	}
}

// runsTableView shapes a job's run history for display, times rendered in the
// job's own timezone. Row order comes from ListRunsForJob: in-flight runs
// (no finished_at yet) first, then finished runs newest-first.
func runsTableView(runs []models.JobRun, tz string) tableView {
	v := tableView{columns: []string{"RUN", "STATUS", "SCHEDULED FOR", "STARTED", "FINISHED", "EXIT", "WORKER", "ERROR"}}
	for _, r := range runs {
		scheduled := r.ScheduledFor
		v.rows = append(v.rows, []string{
			strconv.FormatInt(r.ID, 10),
			r.Status,
			fmtZonedTime(&scheduled, tz),
			fmtZonedTime(r.StartedAt, tz),
			fmtZonedTime(r.FinishedAt, tz),
			fmtInt(r.ExitCode),
			deref(r.WorkerID),
			truncate(deref(r.ErrorMessage), 30),
		})
		// Skipped runs never executed — shade them like disabled jobs.
		v.dim = append(v.dim, r.Status == models.StatusSkipped)
	}
	return v
}

// monitorColumns sizes each column to its widest cell (capped) so the table
// stays readable without hardcoded widths — the provisional layout used until
// the first WindowSizeMsg arrives.
func monitorColumns(v tableView) []table.Column {
	cols := make([]table.Column, len(v.columns))
	for i, w := range naturalWidths(v) {
		cols[i] = table.Column{Title: v.columns[i], Width: clamp(w, 2, 34)}
	}
	return cols
}

// naturalWidths is each column's content width: the widest cell or the
// header, whichever is larger.
func naturalWidths(v tableView) []int {
	widths := make([]int, len(v.columns))
	for i, title := range v.columns {
		w := utf8.RuneCountInString(title)
		for _, row := range v.rows {
			if l := utf8.RuneCountInString(row[i]); l > w {
				w = l
			}
		}
		widths[i] = w
	}
	return widths
}

// Column-fitting parameters: the default table styles pad each cell by one
// space per side; no column shrinks below minColWidth; leftover space goes to
// the NAME column so the table fills the terminal edge-to-edge.
const (
	cellPadding   = 2
	minColWidth   = 4
	stretchColumn = 1 // NAME
)

// fitColumns distributes the terminal width across the columns: natural
// widths when they fit (NAME absorbs the surplus), otherwise the widest
// columns give up space first until the table fits, floored at minColWidth —
// bubbles/table truncates overflowing cells itself, so narrow columns degrade
// to clipped text rather than a broken layout.
func fitColumns(v tableView, termWidth int) []table.Column {
	widths := naturalWidths(v)
	budget := termWidth - cellPadding*len(widths)
	total := 0
	for _, w := range widths {
		total += w
	}

	if surplus := budget - total; surplus > 0 && stretchColumn < len(widths) {
		widths[stretchColumn] += surplus
	}
	for total > budget {
		widest, w := -1, minColWidth
		for i, cw := range widths {
			if cw > w {
				widest, w = i, cw
			}
		}
		if widest == -1 {
			break // every column at minimum; let the terminal clip
		}
		widths[widest]--
		total--
	}

	cols := make([]table.Column, len(widths))
	for i, w := range widths {
		cols[i] = table.Column{Title: v.columns[i], Width: w}
	}
	return cols
}

func monitorRows(v tableView) []table.Row {
	rows := make([]table.Row, len(v.rows))
	for i, r := range v.rows {
		rows[i] = table.Row(r)
	}
	return rows
}

func clamp(n, lo, hi int) int {
	return max(lo, min(n, hi))
}
