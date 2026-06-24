package config

import (
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

type LLMModel struct {
	Name     string `yaml:"name"`
	ModelId  string `yaml:"model_id"`
	Provider string `yaml:"provider"`
}

type MemoryThresholds struct {
	FactConfidenceMin       float64 `yaml:"fact_confidence_min"`
	PreferenceConfidenceMin float64 `yaml:"preference_confidence_min"`
	SemanticDedupCosine     float64 `yaml:"semantic_dedup_cosine"`
}

type MemoryRetrieval struct {
	FactsTopK         int `yaml:"facts_top_k"`
	EpisodesTopK      int `yaml:"episodes_top_k"`
	RecentTraceEvents int `yaml:"recent_trace_events"`
}

type MemoryEpisode struct {
	MinTurns int `yaml:"min_turns"`
}

type MemoryConfig struct {
	Embedding struct {
		Model string `yaml:"model"`
	} `yaml:"embedding"`
	Extractor struct {
		Model string `yaml:"model"`
	} `yaml:"extractor"`
	Thresholds MemoryThresholds `yaml:"thresholds"`
	Retrieval  MemoryRetrieval  `yaml:"retrieval"`
	Episode    MemoryEpisode    `yaml:"episode"`
}

type Config struct {
	DialogTimeout         int          `yaml:"dialog_timeout"`
	MaxConcurrentRequests int          `yaml:"max_concurrent_requests"`
	DefaultModel          LLMModel     `yaml:"default_model"`
	Models                []LLMModel   `yaml:"models"`
	Memory                MemoryConfig `yaml:"memory"`
}

func LoadConfig() (*Config, error) {
	file, err := os.Open("config/application.yaml")
	if err != nil {
		fmt.Printf("Error opening file: %v\n", err)
		return nil, err
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		fmt.Printf("Error reading file: %v\n", err)
		return nil, err
	}
	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		fmt.Printf("Error parsing YAML: %v\n", err)
		return nil, err
	}

	applyMemoryDefaults(&config.Memory)
	return &config, nil
}

func applyMemoryDefaults(m *MemoryConfig) {
	if m.Embedding.Model == "" {
		m.Embedding.Model = "text-embedding-3-small"
	}
	if m.Extractor.Model == "" {
		m.Extractor.Model = "gpt-5.4-nano"
	}
	if m.Thresholds.FactConfidenceMin == 0 {
		m.Thresholds.FactConfidenceMin = 0.7
	}
	if m.Thresholds.PreferenceConfidenceMin == 0 {
		m.Thresholds.PreferenceConfidenceMin = 0.8
	}
	if m.Thresholds.SemanticDedupCosine == 0 {
		m.Thresholds.SemanticDedupCosine = 0.92
	}
	if m.Retrieval.FactsTopK == 0 {
		m.Retrieval.FactsTopK = 5
	}
	if m.Retrieval.EpisodesTopK == 0 {
		m.Retrieval.EpisodesTopK = 2
	}
	if m.Retrieval.RecentTraceEvents == 0 {
		m.Retrieval.RecentTraceEvents = 8
	}
	if m.Episode.MinTurns == 0 {
		m.Episode.MinTurns = 3
	}
}
