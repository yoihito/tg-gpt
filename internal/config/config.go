package config

import (
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
	_ "gopkg.in/yaml.v3"
)

type LLMModel struct {
	Name     string `yaml:"name"`
	ModelId  string `yaml:"model_id"`
	Provider string `yaml:"provider"`
}

type Config struct {
	Models []LLMModel `yaml:"models"`
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

	return &config, nil
}
