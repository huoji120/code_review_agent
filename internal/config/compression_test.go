package config

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestCompressionThresholdReservesOutputAndBoundedTool(t *testing.T) {
	cfg := Config{OpenAI: OpenAIConfig{MaxContextTokens: 32000, MaxOutputTokens: 4096}, Agent: AgentConfig{CompressAtRatio: .9, MaxToolResultChars: 12000, CompressBufferTokens: 4096}}
	limit, err := cfg.CompressionThreshold()
	if err != nil || limit != 13600 {
		t.Fatalf("tool reserve must beat ratio: limit=%d err=%v", limit, err)
	}
	cfg.Agent.CompressBufferTokens = 16000
	limit, err = cfg.CompressionThreshold()
	if err != nil || limit != 11904 {
		t.Fatalf("configured reserve must win: limit=%d err=%v", limit, err)
	}
	cfg.Agent.CompressAtRatio = .25001
	limit, err = cfg.CompressionThreshold()
	if err != nil || limit != 8000 {
		t.Fatalf("ratio must round down: limit=%d err=%v", limit, err)
	}
}

func TestCompressionThresholdRejectsImpossibleBudgets(t *testing.T) {
	base := Config{OpenAI: OpenAIConfig{MaxContextTokens: 32000, MaxOutputTokens: 4096}, Agent: AgentConfig{CompressAtRatio: .75, MaxToolResultChars: 12000}}
	cases := map[string]func(*Config){
		"zero ratio":       func(c *Config) { c.Agent.CompressAtRatio = 0 },
		"unit ratio":       func(c *Config) { c.Agent.CompressAtRatio = 1 },
		"NaN ratio":        func(c *Config) { c.Agent.CompressAtRatio = math.NaN() },
		"infinite ratio":   func(c *Config) { c.Agent.CompressAtRatio = math.Inf(1) },
		"no input room":    func(c *Config) { c.OpenAI.MaxContextTokens = 10000 },
		"zero output":      func(c *Config) { c.OpenAI.MaxOutputTokens = 0 },
		"negative context": func(c *Config) { c.OpenAI.MaxContextTokens = -1 },
		"tiny ratio":       func(c *Config) { c.Agent.CompressAtRatio = .001 },
		"negative reserve": func(c *Config) { c.Agent.CompressBufferTokens = -1 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			change(&cfg)
			if _, err := cfg.CompressionThreshold(); err == nil {
				t.Fatal("impossible admission budget was accepted")
			}
		})
	}
}

func loadBudgetConfig(t *testing.T, text string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoadRejectsExplicitInvalidTokenSettings(t *testing.T) {
	for _, text := range []string{
		"agent:\n  compress_at_ratio: 0\n",
		"agent:\n  compress_at_ratio: .nan\n",
		"openai:\n  max_context_tokens: 0\n",
		"openai:\n  max_output_tokens: 0\n",
		"openai:\n  max_context_tokens: 10000\n",
		"compress_openai:\n  max_context_tokens: 0\n",
		"compress_openai:\n  max_output_tokens: 0\n",
		"compress_openai:\n  max_context_tokens: 2000\n  max_output_tokens: 2000\n",
	} {
		if _, err := loadBudgetConfig(t, text); err == nil {
			t.Errorf("invalid explicit config accepted: %q", text)
		}
	}
}

func TestLoadResponsesDefaultAndExplicitCompatibility(t *testing.T) {
	cfg, err := loadBudgetConfig(t, "workspace: .\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAI.APIInterface != "responses" || cfg.CompressOpenAI.APIInterface != "responses" {
		t.Fatal("default and inherited compressor must use Responses")
	}
	cfg, err = loadBudgetConfig(t, "openai:\n  api_interface: chat_completions\ncompress_openai:\n  api_interface: responses\n  max_context_tokens: 50000\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAI.APIInterface != "chat_completions" || cfg.CompressOpenAI.APIInterface != "responses" || cfg.CompressOpenAI.MaxContextTokens != 50000 {
		t.Fatal("explicit provider interface or compressor limits were overwritten")
	}
}
