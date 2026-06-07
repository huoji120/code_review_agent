package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"code-review-agent/internal/agent"
	"code-review-agent/internal/config"
	"code-review-agent/internal/llm"
	"code-review-agent/internal/paniclog"
	"code-review-agent/internal/prompt"
	"code-review-agent/internal/tools"
	"code-review-agent/internal/tui"
)

func main() {
	defer paniclog.RecoverWithContext("main")

	cfgPath := flag.String("config", "config.yaml", "config file path")
	auditDir := flag.String("dir", "", "directory to audit")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if *auditDir != "" {
		cfg.Workspace = *auditDir
	}

	prompts, err := prompt.Load(cfg.Prompts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load prompts: %v\n", err)
		os.Exit(1)
	}

	client := llm.NewOpenAIClient(cfg.OpenAI)
	compressClient := llm.NewOpenAIClient(cfg.CompressOpenAI)
	registry, err := tools.NewRegistry(cfg.Workspace, cfg.Agent.MaxToolResultChars)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init tools: %v\n", err)
		os.Exit(1)
	}

	runner := agent.NewWithCompressClient(cfg, prompts, client, compressClient, registry)
	program := tea.NewProgram(tui.New(runner, cfg, *auditDir), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "run tui: %v\n", err)
		os.Exit(1)
	}
}
