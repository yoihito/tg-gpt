package services

import (
	"context"
	"fmt"
	"os"

	openai "github.com/sashabaranov/go-openai"
	"vadimgribanov.com/tg-gpt/internal/adapters"
	"vadimgribanov.com/tg-gpt/internal/config"
	"vadimgribanov.com/tg-gpt/internal/llm"
	"vadimgribanov.com/tg-gpt/internal/vendors/anthropic"
)

type LLMClientProxy struct {
	supportedModels map[string]config.LLMModel
	providers       map[llm.Provider]ProviderClient
	OpenaiClient    *openai.Client
}

type ProviderClient interface {
	llm.Client
}

func NewLLMClientProxy() *LLMClientProxy {
	return &LLMClientProxy{supportedModels: make(map[string]config.LLMModel), providers: make(map[llm.Provider]ProviderClient)}
}

func NewClientProxyFromConfig(config *config.Config) *LLMClientProxy {
	proxy := NewLLMClientProxy()
	client := openai.NewClient(os.Getenv("OPENAI_API_KEY"))
	proxy.OpenaiClient = client
	anthropicClient := anthropic.NewClient(os.Getenv("ANTHROPIC_API_KEY"))
	proxy.registerProvider(adapters.NewOpenaiAdapter(client))
	proxy.registerProvider(adapters.NewAnthropicAdapter(anthropicClient))
	for _, model := range config.Models {
		proxy.registerAvailableModel(model)
	}
	return proxy
}

func (p *LLMClientProxy) IsClientRegistered(name string) bool {
	_, ok := p.supportedModels[name]
	return ok
}

func (p *LLMClientProxy) ListModels() []string {
	models := make([]string, 0, len(p.supportedModels))
	for model := range p.supportedModels {
		models = append(models, model)
	}
	return models
}

func (p *LLMClientProxy) registerProvider(client ProviderClient) {
	p.providers[client.Provider()] = client
}

func (p *LLMClientProxy) registerAvailableModel(modelConfig config.LLMModel) {
	p.supportedModels[modelConfig.ModelId] = modelConfig
}

func (p *LLMClientProxy) getClient(modelId string) (ProviderClient, error) {
	if _, ok := p.supportedModels[modelId]; !ok {
		return nil, fmt.Errorf("client with modelId %s not found", modelId)
	}
	provider := llm.Provider(p.supportedModels[modelId].Provider)
	if _, ok := p.providers[provider]; !ok {
		return nil, fmt.Errorf("provider with name %s not found", provider)
	}

	return p.providers[provider], nil
}

func (p *LLMClientProxy) Stream(ctx context.Context, request llm.Request) (llm.Stream, error) {
	client, err := p.getClient(request.Model)
	if err != nil {
		return nil, err
	}
	return client.Stream(ctx, request)
}
