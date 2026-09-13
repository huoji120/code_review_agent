package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	frameColor    = lipgloss.AdaptiveColor{Light: "246", Dark: "240"}
	focusColor    = lipgloss.AdaptiveColor{Light: "30", Dark: "80"}
	accentStyle   = lipgloss.NewStyle().Foreground(focusColor).Bold(true)
	mutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "241", Dark: "247"})
	borderStyle   = lipgloss.NewStyle().Foreground(frameColor)
	selectedStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "23", Dark: "159"}).Background(lipgloss.AdaptiveColor{Light: "195", Dark: "23"}).Bold(true)
	dangerStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "160", Dark: "203"}).Bold(true)
	warningStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "130", Dark: "221"})
	successStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "114"})
)

// Painting is the final rendering step: callers sanitize, wrap and clip first.
// Body text keeps the terminal's default foreground instead of being dimmed.
func paintLine(text string) string {
	trimmed := strings.TrimSpace(text)
	selected := strings.HasPrefix(trimmed, ">")
	if selected {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
	}
	// Finding list rows place their severity after a stable numeric ID.
	if strings.HasPrefix(trimmed, "#") {
		if _, rest, ok := strings.Cut(trimmed, " "); ok {
			trimmed = strings.TrimSpace(rest)
		}
	}
	status := ""
	if strings.HasPrefix(trimmed, "[") {
		if end := strings.IndexByte(trimmed, ']'); end >= 0 {
			status = strings.ToLower(trimmed[1:end])
		}
	}
	var style lipgloss.Style
	switch {
	case status == "critical", status == "high", status == "failed", status == "失败", strings.HasPrefix(trimmed, "! "):
		style = dangerStyle
	case status == "medium", status == "waiting", status == "paused", status == "等待", strings.HasPrefix(trimmed, "保留缺口："):
		style = warningStyle
	case status == "completed", status == "完成", status == "low":
		style = successStyle
	case strings.HasPrefix(trimmed, "──"):
		style = accentStyle
		if strings.Trim(trimmed, "─ ") == "" {
			style = borderStyle
		}
	case status == "+", status == "-", status == "running", status == "运行", strings.HasPrefix(trimmed, "标题："), strings.HasPrefix(trimmed, "回复 #"):
		style = accentStyle
	case status == "idle", status == "pending", status == "cancelled", status == "待开始", status == "停止", strings.HasPrefix(trimmed, "作者："), strings.HasPrefix(trimmed, "更新："), strings.HasPrefix(trimmed, "范围："), strings.HasPrefix(trimmed, "提示："), strings.HasPrefix(trimmed, "位置 ·"):
		style = mutedStyle
	default:
		if selected {
			return selectedStyle.Render(text)
		}
		return text
	}
	if selected {
		style = selectedStyle.Foreground(style.GetForeground())
	}
	return style.Render(text)
}

func paintAccent(text string) string { return accentStyle.Render(text) }
func paintMuted(text string) string  { return mutedStyle.Render(text) }
func paintBorder(text string) string { return borderStyle.Render(text) }
