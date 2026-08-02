// Package tui provides shared terminal UI theming for interactive prompts.
package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// Toad brand colors.
var (
	ColorPrimary   = lipgloss.Color("#4CAF50") // toad green
	ColorSecondary = lipgloss.Color("#81C784") // light green
	ColorAccent    = lipgloss.Color("#FFC107") // amber
	ColorDim       = lipgloss.Color("#6B7280") // muted gray
	ColorError     = lipgloss.Color("#FF5252") // error red
	ColorBorder    = lipgloss.Color("#3F3F46") // zinc-700
	ColorSuccess   = lipgloss.Color("#4CAF50") // green (same as primary)
	ColorWhite     = lipgloss.Color("#FFFFFF")
	ColorSubtle    = lipgloss.Color("#4A4A4A") // very dim
)

// Shared styles for the setup wizard.
var (
	TitleStyle = lipgloss.NewStyle().
			Foreground(ColorPrimary).
			Bold(true)

	SelectedStyle = lipgloss.NewStyle().
			Foreground(ColorPrimary).
			Bold(true)

	CursorStyle = lipgloss.NewStyle().
			Foreground(ColorPrimary).
			Bold(true)

	DimStyle = lipgloss.NewStyle().
			Foreground(ColorDim)

	ErrorStyle = lipgloss.NewStyle().
			Foreground(ColorError)

	SuccessStyle = lipgloss.NewStyle().
			Foreground(ColorSuccess).
			Bold(true)

	HelpStyle = lipgloss.NewStyle().
			Foreground(ColorDim)

	AccentStyle = lipgloss.NewStyle().
			Foreground(ColorAccent)
)

// StyledMessage prints a styled message for non-form terminal output.
func StyledMessage(msg string) string {
	style := lipgloss.NewStyle().
		Foreground(ColorPrimary).
		Bold(true)
	return fmt.Sprintf("\n  %s\n", style.Render(msg))
}

// RenderProgressBar renders a step progress indicator.
// Steps are shown as: ✓ Done → [Current] → Upcoming
func RenderProgressBar(steps []string, current int) string {
	var b strings.Builder
	for i, step := range steps {
		if i == current {
			b.WriteString(SelectedStyle.Render(fmt.Sprintf("[%s]", step)))
		} else if i < current {
			b.WriteString(SuccessStyle.Render("✓ " + step))
		} else {
			b.WriteString(DimStyle.Render("  " + step))
		}
		if i < len(steps)-1 {
			b.WriteString(DimStyle.Render("  →  "))
		}
	}
	return b.String()
}
