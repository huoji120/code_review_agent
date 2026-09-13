package config

import "testing"

func TestThinkingConfigurationRejectsDisableAndKeepsIndependentEffort(t *testing.T) {
	for _, section := range []string{"openai", "compress_openai"} {
		if _, err := loadBudgetConfig(t, section+":\n  thinking:\n    type: disabled\n"); err == nil {
			t.Fatal("disabled THINK accepted")
		}
		if _, err := loadBudgetConfig(t, section+":\n  reasoning_effort: medium\n"); err == nil {
			t.Fatal("unsupported effort accepted")
		}
	}
	cfg, err := loadBudgetConfig(t, "openai:\n  reasoning_effort: max\ncompress_openai:\n  reasoning_effort: low\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAI.ReasoningEffort != "max" || cfg.CompressOpenAI.ReasoningEffort != "low" || cfg.OpenAI.Thinking.Type != "enabled" || cfg.CompressOpenAI.Thinking.Type != "enabled" {
		t.Fatal("explicit per-model effort overridden or thinking disabled")
	}
}
