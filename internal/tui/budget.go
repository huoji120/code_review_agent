package tui

import (
	"fmt"
	"strconv"
	"strings"

	"code-review-agent/internal/agent"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type budgetForm struct {
	infinite bool
	selected int
	fields   [3]textinput.Model
}

func newBudgetForm(b agent.BudgetStatus) *budgetForm {
	f := &budgetForm{infinite: b.InfiniteMode}
	values := []string{strconv.Itoa(b.Hours), strconv.Itoa(b.Minutes), strconv.FormatInt(b.TokenLimit, 10)}
	for i := range f.fields {
		input := textinput.New()
		input.Prompt = ""
		input.Placeholder = "0"
		input.CharLimit = 20
		input.SetValue(values[i])
		f.fields[i] = input
	}
	return f
}

func (m *Model) updateBudget(msg tea.Msg) tea.Cmd {
	state := m.modal
	f := state.budgetForm
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc", "ctrl+[":
			m.pendingDir = ""
			return m.closeModal()
		case "ctrl+c":
			cmd, _ := m.handleKey(key)
			return cmd
		case "tab", "down", "shift+tab", "up":
			step := 1
			if key.String() == "shift+tab" || key.String() == "up" {
				step = -1
			}
			for i := range f.fields {
				f.fields[i].Blur()
			}
			f.selected = (f.selected + step + 4) % 4
			if f.selected > 0 {
				return f.fields[f.selected-1].Focus()
			}
			return nil
		case "left", "right", " ", "enter":
			if f.selected == 0 {
				f.infinite = !f.infinite
				state.err = ""
				return nil
			}
		case "ctrl+s":
			h, e1 := strconv.Atoi(strings.TrimSpace(f.fields[0].Value()))
			minutes, e2 := strconv.Atoi(strings.TrimSpace(f.fields[1].Value()))
			tokens, e3 := strconv.ParseInt(strings.TrimSpace(f.fields[2].Value()), 10, 64)
			if e1 != nil || e2 != nil || e3 != nil || h < 0 || minutes < 0 || tokens < 0 {
				state.err = "请输入非负整数；0 表示不限"
				return nil
			}
			if err := m.runner.ConfigureBudget(f.infinite, h, minutes, tokens); err != nil {
				state.err = err.Error()
				return nil
			}
			m.budgetCfg = m.runner.BudgetStatus()
			dir := m.pendingDir
			m.pendingDir = ""
			cmd := m.closeModal()
			if dir != "" {
				return m.startDirectory(dir)
			}
			return cmd
		}
	}
	if _, mouse := msg.(tea.MouseMsg); mouse {
		return nil
	}
	if f.selected > 0 {
		before := f.fields[f.selected-1].Value()
		var cmd tea.Cmd
		f.fields[f.selected-1], cmd = f.fields[f.selected-1].Update(msg)
		if before != f.fields[f.selected-1].Value() {
			state.err = ""
		}
		return cmd
	}
	return nil
}

func (m Model) budgetLines(width, height int) []string {
	f := m.modal.budgetForm
	lines := []string{paintMuted(fitLine("设置整个团队的运行上限", width)), ""}
	starts := [4]int{}
	mode := "[普通]  无限审计"
	note := "普通：允许共识结束，不启用预算限额"
	if f.infinite {
		mode = "普通  [无限审计]"
		note = "无限审计：不自主结束，仍受预算约束"
	}
	labels := []string{"运行模式", "小时", "分钟", "Token 上限"}
	for i, label := range labels {
		starts[i] = len(lines)
		marker := "  "
		if f.selected == i {
			marker = "> "
		}
		lines = append(lines, paintLine(fitLine(marker+label, width)))
		value := mode
		if i > 0 {
			value = f.fields[i-1].View()
		}
		// Input text is numeric user input; textinput owns its cursor ANSI.
		if i == 0 {
			value = paintAccent(fitLine(value, max(1, width-2)))
		}
		lines = append(lines, "  "+value, "")
	}
	for _, text := range []string{note, "0 = 不限 · 小时与分钟相加", fmt.Sprintf("已用 %s · %d token（保存后继续累计）", m.budgetCfg.Elapsed.Round(1e9), m.budgetCfg.UsedTokens)} {
		for _, line := range wrapLines(text, width) {
			lines = append(lines, paintMuted(line))
		}
	}
	start := 0
	if starts[f.selected]+2 > height {
		start = starts[f.selected] + 2 - height
	}
	return viewport(lines, start, height, false)
}
