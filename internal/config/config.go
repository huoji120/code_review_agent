package config

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Workspace      string       `yaml:"workspace"`
	OpenAI         OpenAIConfig `yaml:"openai"`
	CompressOpenAI OpenAIConfig `yaml:"compress_openai"`
	Prompts        PromptConfig `yaml:"prompts"`
	Agent          AgentConfig  `yaml:"agent"`
}

type OpenAIConfig struct {
	BaseURL          string          `yaml:"base_url"`
	APIInterface     string          `yaml:"api_interface"`
	APIKey           string          `yaml:"api_key"`
	APIKeyEnv        string          `yaml:"api_key_env"`
	Model            string          `yaml:"model"`
	Temperature      float64         `yaml:"temperature"`
	TopP             float64         `yaml:"top_p"`
	MaxContextTokens int             `yaml:"max_context_tokens"`
	MaxOutputTokens  int             `yaml:"max_output_tokens"`
	TimeoutSeconds   int             `yaml:"timeout_seconds"`
	Stream           bool            `yaml:"stream"`
	Thinking         *ThinkingConfig `yaml:"thinking"`
	ReasoningEffort  string          `yaml:"reasoning_effort"`
}

type ThinkingConfig struct {
	Type string `yaml:"type" json:"type"`
}

func validateReasoning(cfg OpenAIConfig, section string) error {
	if cfg.Thinking != nil && cfg.Thinking.Type != "enabled" {
		return fmt.Errorf("%s.thinking.type must be enabled; disabled is not supported", section)
	}
	switch cfg.ReasoningEffort {
	case "", "low", "high", "max":
		return nil
	default:
		return fmt.Errorf("%s.reasoning_effort must be low, high or max", section)
	}
}

type PromptConfig struct {
	System       string `yaml:"system"`
	PlanSystem   string `yaml:"plan_system"`
	Compress     string `yaml:"compress"`
	SkillsDir    string `yaml:"skills_dir"`
	TemplatesDir string `yaml:"templates_dir"`
}

type AgentConfig struct {
	MaxTurns                 int     `yaml:"max_turns"`
	SessionDir               string  `yaml:"session_dir"`
	LogSession               bool    `yaml:"log_session"`
	LogSessionDir            string  `yaml:"log_session_dir"`
	RetryAttempts            int     `yaml:"retry_attempts"`
	CompressAtRatio          float64 `yaml:"compress_at_ratio"`
	CompressBufferTokens     int     `yaml:"compress_buffer_tokens"`
	AutoPlan                 bool    `yaml:"auto_plan"`
	MaxToolResultChars       int     `yaml:"max_tool_result_chars"`
	ReconAgents              int     `yaml:"recon_agents"`
	AuditAgents              int     `yaml:"audit_agents"`
	ForumWaitSeconds         int     `yaml:"forum_wait_seconds"`
	ModeratorEnabled         *bool   `yaml:"moderator_enabled"`
	ModeratorIntervalSeconds int     `yaml:"moderator_interval_seconds"`
	InfiniteMode             bool    `yaml:"infinite_mode"`
	BudgetHours              int     `yaml:"budget_hours"`
	BudgetMinutes            int     `yaml:"budget_minutes"`
	BudgetTokens             int64   `yaml:"budget_tokens"`
}

// BudgetDuration keeps explicit zero as unlimited and rejects overflow before
// converting user-supplied hours/minutes to a timer duration.
func (cfg AgentConfig) BudgetDuration() (time.Duration, error) {
	const maxMinutes = int64(math.MaxInt64) / int64(time.Minute)
	if cfg.BudgetHours < 0 || cfg.BudgetMinutes < 0 || cfg.BudgetTokens < 0 {
		return 0, fmt.Errorf("audit budgets must be nonnegative; 0 means unlimited")
	}
	if int64(cfg.BudgetMinutes) > maxMinutes || int64(cfg.BudgetHours) > (maxMinutes-int64(cfg.BudgetMinutes))/60 {
		return 0, fmt.Errorf("audit time budget overflows time.Duration")
	}
	return time.Duration(int64(cfg.BudgetHours)*60+int64(cfg.BudgetMinutes)) * time.Minute, nil
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	// Seed required defaults before decoding so explicit zero remains invalid.
	cfg := Config{
		OpenAI: OpenAIConfig{MaxContextTokens: 32000, MaxOutputTokens: 4096},
		Agent:  AgentConfig{CompressAtRatio: 0.75, BudgetHours: 8},
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return Config{}, err
	}
	if err := document.Decode(&cfg); err != nil {
		return Config{}, err
	}
	// Missing compressor limits inherit from the main model; explicit zero is
	// an invalid limit, not an instruction to silently substitute a default.
	if len(document.Content) > 0 {
		root := document.Content[0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value != "compress_openai" {
				continue
			}
			section := root.Content[i+1]
			for j := 0; j+1 < len(section.Content); j += 2 {
				key := section.Content[j].Value
				if (key == "max_context_tokens" && cfg.CompressOpenAI.MaxContextTokens <= 0) || (key == "max_output_tokens" && cfg.CompressOpenAI.MaxOutputTokens <= 0) {
					return Config{}, fmt.Errorf("compress_openai.%s must be positive", key)
				}
			}
		}
	}
	applyDefaults(&cfg)
	if err := validateReasoning(cfg.OpenAI, "openai"); err != nil {
		return Config{}, err
	}
	if err := validateReasoning(cfg.CompressOpenAI, "compress_openai"); err != nil {
		return Config{}, err
	}
	if _, err := cfg.Agent.BudgetDuration(); err != nil {
		return Config{}, err
	}
	if cfg.Agent.ReconAgents < 1 || cfg.Agent.ReconAgents > 666 || cfg.Agent.AuditAgents < 1 || cfg.Agent.AuditAgents > 666 {
		return Config{}, fmt.Errorf("agent.recon_agents and agent.audit_agents must be between 1 and 666")
	}
	if cfg.Agent.ModeratorIntervalSeconds < 1 || cfg.Agent.ModeratorIntervalSeconds > 86400 {
		return Config{}, fmt.Errorf("agent.moderator_interval_seconds must be between 1 and 86400")
	}
	if cfg.Agent.ForumWaitSeconds < 1 || cfg.Agent.ForumWaitSeconds > 120 {
		return Config{}, fmt.Errorf("agent.forum_wait_seconds must be between 1 and 120")
	}
	if cfg.Agent.MaxToolResultChars < 1024 || cfg.Agent.MaxToolResultChars > 65536 {
		return Config{}, fmt.Errorf("agent.max_tool_result_chars must be between 1024 and 65536 (serialized UTF-8 bytes)")
	}
	if _, err := cfg.CompressionThreshold(); err != nil {
		return Config{}, err
	}
	if _, err := cfg.CompressionInputBudget(); err != nil {
		return Config{}, err
	}
	resolveAPIKey(&cfg.OpenAI)
	resolveAPIKey(&cfg.CompressOpenAI)
	return cfg, nil
}

// CompressionThreshold is an estimated-token admission limit, not tokenizer usage.
// Tool results are bounded UTF-8 bytes; reserve one token per byte conservatively,
// plus the notification allowance and message framing, independently of the ratio.
func (cfg Config) CompressionThreshold() (int, error) {
	context, output := cfg.OpenAI.MaxContextTokens, cfg.OpenAI.MaxOutputTokens
	ratio := cfg.Agent.CompressAtRatio
	if context <= 0 || output <= 0 || output >= context {
		return 0, fmt.Errorf("openai token limits must be positive with output below context")
	}
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 || ratio >= 1 {
		return 0, fmt.Errorf("agent.compress_at_ratio must be finite and strictly between 0 and 1")
	}
	if cfg.Agent.CompressBufferTokens < 0 || cfg.Agent.MaxToolResultChars < 0 || cfg.Agent.MaxToolResultChars > 65536 {
		return 0, fmt.Errorf("compression reserve and tool result bounds are invalid")
	}
	toolBytes := cfg.Agent.MaxToolResultChars
	if toolBytes < 1024 {
		toolBytes = 1024
	}
	reserve := toolBytes + 2048 + 256
	if cfg.Agent.CompressBufferTokens > reserve {
		reserve = cfg.Agent.CompressBufferTokens
	}
	if reserve >= context-output {
		return 0, fmt.Errorf("openai context cannot fit output plus compression/tool/notification reserve")
	}
	limit := context - output - reserve
	ratioLimit := math.Floor(float64(context) * ratio)
	if ratioLimit < float64(limit) {
		limit = int(ratioLimit)
	}
	if limit < 1024 {
		return 0, fmt.Errorf("effective compression threshold must leave at least 1024 estimated input tokens")
	}
	return limit, nil
}

// CompressionInputBudget reserves the configured summarizer output in full.
func (cfg Config) CompressionInputBudget() (int, error) {
	context, output := cfg.CompressOpenAI.MaxContextTokens, cfg.CompressOpenAI.MaxOutputTokens
	if context <= 0 || output <= 0 || output >= context {
		return 0, fmt.Errorf("compress_openai token limits must be positive with output below context")
	}
	return context - output, nil
}

func resolveAPIKey(cfg *OpenAIConfig) {
	if cfg.APIKey != "" || cfg.APIKeyEnv == "" {
		return
	}
	if looksLikeAPIKey(cfg.APIKeyEnv) {
		cfg.APIKey = cfg.APIKeyEnv
	} else {
		cfg.APIKey = os.Getenv(cfg.APIKeyEnv)
	}
}

func looksLikeAPIKey(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "sk-") {
		return true
	}
	if strings.Contains(value, "-") && len(value) >= 20 {
		return true
	}
	return false
}

func applyDefaults(cfg *Config) {
	if cfg.OpenAI.Thinking == nil {
		cfg.OpenAI.Thinking = &ThinkingConfig{Type: "enabled"}
	}
	if cfg.OpenAI.ReasoningEffort == "" {
		cfg.OpenAI.ReasoningEffort = "high"
	}
	if cfg.Workspace == "" {
		cfg.Workspace = "."
	}
	if cfg.OpenAI.BaseURL == "" {
		cfg.OpenAI.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.OpenAI.APIInterface == "" {
		cfg.OpenAI.APIInterface = "responses"
	}
	if cfg.OpenAI.Model == "" {
		cfg.OpenAI.Model = "gpt-4o-mini"
	}
	if cfg.OpenAI.TopP == 0 {
		cfg.OpenAI.TopP = 1
	}
	if cfg.OpenAI.TimeoutSeconds == 0 {
		cfg.OpenAI.TimeoutSeconds = 10
	}
	applyCompressOpenAIDefaults(cfg)
	if cfg.Prompts.System == "" {
		cfg.Prompts.System = "prompts/system.md"
	}
	if cfg.Prompts.PlanSystem == "" {
		cfg.Prompts.PlanSystem = "prompts/plan_system.md"
	}
	if cfg.Prompts.Compress == "" {
		cfg.Prompts.Compress = "prompts/compress.md"
	}
	if cfg.Prompts.SkillsDir == "" {
		cfg.Prompts.SkillsDir = "skills"
	}
	if cfg.Prompts.TemplatesDir == "" {
		cfg.Prompts.TemplatesDir = "prompts/templates"
	}
	if cfg.Agent.SessionDir == "" {
		cfg.Agent.SessionDir = "sessions"
	}
	if cfg.Agent.LogSessionDir == "" {
		cfg.Agent.LogSessionDir = "log_sessions"
	}
	if cfg.Agent.RetryAttempts == 0 {
		cfg.Agent.RetryAttempts = 3
	}
	if cfg.Agent.CompressBufferTokens == 0 {
		cfg.Agent.CompressBufferTokens = cfg.OpenAI.MaxOutputTokens
		if cfg.Agent.CompressBufferTokens < 4096 {
			cfg.Agent.CompressBufferTokens = 4096
		}
	}
	if cfg.Agent.MaxToolResultChars == 0 {
		cfg.Agent.MaxToolResultChars = 12000
	}
	if cfg.Agent.ReconAgents == 0 {
		cfg.Agent.ReconAgents = 4
	}
	if cfg.Agent.AuditAgents == 0 {
		cfg.Agent.AuditAgents = 4
	}
	if cfg.Agent.ModeratorIntervalSeconds == 0 {
		cfg.Agent.ModeratorIntervalSeconds = 60
	}
	if cfg.Agent.ForumWaitSeconds == 0 {
		cfg.Agent.ForumWaitSeconds = 60
	}
}

func (cfg AgentConfig) ModeratorIsEnabled() bool {
	return cfg.ModeratorEnabled == nil || *cfg.ModeratorEnabled
}

func applyCompressOpenAIDefaults(cfg *Config) {
	compress := cfg.CompressOpenAI
	if compress == (OpenAIConfig{}) {
		cfg.CompressOpenAI = cfg.OpenAI
		cfg.CompressOpenAI.Stream = false
		return
	}
	if compress.BaseURL == "" {
		compress.BaseURL = cfg.OpenAI.BaseURL
	}
	if compress.APIInterface == "" {
		compress.APIInterface = cfg.OpenAI.APIInterface
	}
	if compress.Thinking == nil {
		compress.Thinking = &ThinkingConfig{Type: "enabled"}
	}
	if compress.ReasoningEffort == "" {
		compress.ReasoningEffort = "high"
	}
	if compress.APIKey == "" && compress.APIKeyEnv == "" {
		compress.APIKey = cfg.OpenAI.APIKey
		compress.APIKeyEnv = cfg.OpenAI.APIKeyEnv
	}
	if compress.Model == "" {
		compress.Model = cfg.OpenAI.Model
	}
	if compress.TopP == 0 {
		compress.TopP = cfg.OpenAI.TopP
	}
	if compress.MaxContextTokens == 0 {
		compress.MaxContextTokens = cfg.OpenAI.MaxContextTokens
	}
	if compress.MaxOutputTokens == 0 {
		compress.MaxOutputTokens = cfg.OpenAI.MaxOutputTokens
	}
	if compress.TimeoutSeconds == 0 {
		compress.TimeoutSeconds = cfg.OpenAI.TimeoutSeconds
	}
	compress.Stream = false
	cfg.CompressOpenAI = compress
}
