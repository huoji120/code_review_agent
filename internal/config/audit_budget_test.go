package config

import (
	"testing"
	"time"
)

func TestInfiniteBudgetDefaultsUnlimitedAndInvalidValues(t *testing.T) {
	cfg, err := loadBudgetConfig(t, "workspace: .\n")
	if err != nil {
		t.Fatal(err)
	}
	duration, err := cfg.Agent.BudgetDuration()
	if err != nil || duration != 8*time.Hour || cfg.Agent.InfiniteMode || cfg.Agent.BudgetTokens != 0 {
		t.Fatalf("unexpected default budget: %+v %v", cfg.Agent, err)
	}
	cfg, err = loadBudgetConfig(t, "agent:\n  infinite_mode: true\n  budget_hours: 0\n  budget_minutes: 0\n  budget_tokens: 0\n")
	if err != nil {
		t.Fatal(err)
	}
	duration, _ = cfg.Agent.BudgetDuration()
	if duration != 0 || !cfg.Agent.InfiniteMode {
		t.Fatal("explicit unlimited replaced with defaults")
	}
	for _, field := range []string{"budget_hours: -1", "budget_minutes: -1", "budget_tokens: -1", "budget_hours: 9223372036854775807"} {
		if _, err := loadBudgetConfig(t, "agent:\n  "+field+"\n"); err == nil {
			t.Fatalf("invalid budget accepted: %s", field)
		}
	}
}
