package tui

import (
	"fmt"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

// Toad brand colors.
var (
	ColorPrimary = lipgloss.Color("#4CAF50") // toad green
	ColorDim     = lipgloss.Color("#888888") // muted gray
	ColorError   = lipgloss.Color("#FF5252") // error red
)

// ToadTheme returns a huh theme with toad-green accents.
func ToadTheme() *huh.Theme {
	t := huh.ThemeBase()

	// Focused state — green accents
	t.Focused.Title = t.Focused.Title.Foreground(ColorPrimary)
	t.Focused.FocusedButton = t.Focused.FocusedButton.Background(ColorPrimary).Foreground(lipgloss.Color("#FFFFFF"))
	t.Focused.SelectSelector = t.Focused.SelectSelector.Foreground(ColorPrimary)
	t.Focused.MultiSelectSelector = t.Focused.MultiSelectSelector.Foreground(ColorPrimary)
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(ColorPrimary)
	t.Focused.SelectedPrefix = t.Focused.SelectedPrefix.Foreground(ColorPrimary)
	t.Focused.TextInput.Cursor = t.Focused.TextInput.Cursor.Foreground(ColorPrimary)
	t.Focused.NoteTitle = t.Focused.NoteTitle.Foreground(ColorPrimary)
	t.Focused.Next = t.Focused.Next.Background(ColorPrimary).Foreground(lipgloss.Color("#FFFFFF"))
	t.Focused.Description = t.Focused.Description.Foreground(ColorDim)

	return t
}

// StyledMessage prints a styled message for non-form terminal output.
func StyledMessage(msg string) string {
	style := lipgloss.NewStyle().
		Foreground(ColorPrimary).
		Bold(true)
	return fmt.Sprintf("\n  %s\n", style.Render(msg))
}
