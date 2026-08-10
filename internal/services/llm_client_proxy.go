package services

import (
	"context"
	"fmt"
	"os"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
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
	client := openai.NewClient(option.WithAPIKey(os.Getenv("OPENAI_API_KEY")))
	proxy.OpenaiClient = &client
	anthropicClient := anthropic.NewClient(os.Getenv("ANTHROPIC_API_KEY"))
	deepInfraClient := openai.NewClient(
		option.WithAPIKey(os.Getenv("DEEPINFRA_API_KEY")),
		option.WithBaseURL("https://api.deepinfra.com/v1/openai/"),
	)
	proxy.registerProvider(adapters.NewOpenaiAdapter(&client))
	proxy.registerProvider(adapters.NewAnthropicAdapter(anthropicClient))
	proxy.registerProvider(adapters.NewDeepInfraAdapter(&deepInfraClient))
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

// Capabilities reports what modelId supports. Unknown models report the zero value
// (nothing supported) rather than erroring, since callers use this to decide what to
// omit from a request, not to validate the model itself.
func (p *LLMClientProxy) Capabilities(modelId string) llm.Capabilities {
	client, err := p.getClient(modelId)
	if err != nil {
		return llm.Capabilities{}
	}
	return client.Capabilities(modelId)
}
