package main

import "github.com/charmbracelet/lipgloss"

// accentColor is the single accent used across every styled ("pretty") shell
// message — the banner, the login/logout boxes, help headers. Defining it once
// keeps all pretty output on the banner's color scheme; change it here and the
// whole shell follows.
var accentColor = lipgloss.Color("63")

// accentText renders bold text in the accent color (banner titles, box labels).
func accentText(s string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(accentColor).Render(s)
}

// faintText renders de-emphasized secondary text — matches the banner subtitle.
func faintText(s string) string {
	return lipgloss.NewStyle().Faint(true).Render(s)
}

// upColor marks a running daemon in status lines — the one place the shell
// steps outside the single accent, since up/down is a semantic signal.
var upColor = lipgloss.Color("42")

// statusUp renders the "running" badge for daemon status displays.
func statusUp() string {
	return lipgloss.NewStyle().Foreground(upColor).Render("● up")
}

// statusDown renders the "not running" badge; faint, matching the shell's
// dimming convention for things currently out of reach.
func statusDown() string {
	return faintText("○ down")
}

// accentBox draws content inside a rounded border tinted with the accent color,
// the shared frame for the login-failed and logged-out messages.
func accentBox(content string) string {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accentColor).
		Padding(0, 2).
		Render(content)
}
