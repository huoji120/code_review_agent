package config

import (
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
	BaseURL          string  `yaml:"base_url"`
	APIInterface     string  `yaml:"api_interface"`
	APIKey           string  `yaml:"api_key"`
	APIKeyEnv        string  `yaml:"api_key_env"`
	Model            string  `yaml:"model"`
	Temperature      float64 `yaml:"temperature"`
	TopP             float64 `yaml:"top_p"`
	MaxContextTokens int     `yaml:"max_context_tokens"`
	MaxOutputTokens  int     `yaml:"max_output_tokens"`
	TimeoutSeconds   int     `yaml:"timeout_seconds"`
	Stream           bool    `yaml:"stream"`
}

type PromptConfig struct {
	System       string `yaml:"system"`
	Compress     string `yaml:"compress"`
	SkillsDir    string `yaml:"skills_dir"`
	TemplatesDir string `yaml:"templates_dir"`
}

type AgentConfig struct {
	MaxTurns             int     `yaml:"max_turns"`
	SummaryInterval      int     `yaml:"summary_interval"`
	AutoSaveInterval     int     `yaml:"auto_save_interval"`
	SessionDir           string  `yaml:"session_dir"`
	LogSession           bool    `yaml:"log_session"`
	LogSessionDir        string  `yaml:"log_session_dir"`
	RetryAttempts        int     `yaml:"retry_attempts"`
	CompressAtRatio      float64 `yaml:"compress_at_ratio"`
	CompressBufferTokens int     `yaml:"compress_buffer_tokens"`
	AutoPlan             bool    `yaml:"auto_plan"`
	MaxToolResultChars   int     `yaml:"max_tool_result_chars"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	applyDefaults(&cfg)
	resolveAPIKey(&cfg.OpenAI)
	resolveAPIKey(&cfg.CompressOpenAI)
	return cfg, nil
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
	if cfg.Workspace == "" {
		cfg.Workspace = "."
	}
	if cfg.OpenAI.BaseURL == "" {
		cfg.OpenAI.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.OpenAI.APIInterface == "" {
		cfg.OpenAI.APIInterface = "chat_completions"
	}
	if cfg.OpenAI.Model == "" {
		cfg.OpenAI.Model = "gpt-4o-mini"
	}
	if cfg.OpenAI.TopP == 0 {
		cfg.OpenAI.TopP = 1
	}
	if cfg.OpenAI.MaxContextTokens == 0 {
		cfg.OpenAI.MaxContextTokens = 32000
	}
	if cfg.OpenAI.MaxOutputTokens == 0 {
		cfg.OpenAI.MaxOutputTokens = 4096
	}
	if cfg.OpenAI.TimeoutSeconds == 0 {
		cfg.OpenAI.TimeoutSeconds = int((120 * time.Second).Seconds())
	}
	applyCompressOpenAIDefaults(cfg)
	if cfg.Prompts.System == "" {
		cfg.Prompts.System = "prompts/system.md"
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
	if cfg.Agent.CompressAtRatio == 0 {
		cfg.Agent.CompressAtRatio = 0.75
	}
	if cfg.Agent.SummaryInterval == 0 {
		cfg.Agent.SummaryInterval = 10
	}
	if cfg.Agent.AutoSaveInterval == 0 {
		cfg.Agent.AutoSaveInterval = 5
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
}

func applyCompressOpenAIDefaults(cfg *Config) {
	compress := cfg.CompressOpenAI
	if compress.BaseURL == "" && compress.APIKey == "" && compress.APIKeyEnv == "" && compress.Model == "" {
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
