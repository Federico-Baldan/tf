package main

import "github.com/charmbracelet/lipgloss"

var (
	stGreen   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	stYellow  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	stRed     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	stMagenta = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	stCyan    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	stDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	stBold    = lipgloss.NewStyle().Bold(true)
	stSection = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("4"))
	stCursor  = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	// note text on the highlighted row: dim, but on the cursor background
	stCursorNote = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Background(lipgloss.Color("236"))
	stRedBold    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func actionGlyph(action string) (string, lipgloss.Style) {
	switch action {
	case actCreate:
		return "+", stGreen
	case actUpdate:
		return "~", stYellow
	case actDelete:
		return "-", stRed
	case actReplace:
		return "±", stMagenta
	case actRead, "refresh":
		return "↻", stCyan
	case actImport:
		return "↓", stCyan
	case actMove:
		return "→", stDim
	case actForget:
		return "⊘", stYellow
	case actInvoke:
		return "▶", stCyan
	default:
		return "·", stDim
	}
}

// hint renders a key legend entry: bold key, dim label.
func hint(key, label string) string {
	return stBold.Render(key) + " " + stDim.Render(label)
}
