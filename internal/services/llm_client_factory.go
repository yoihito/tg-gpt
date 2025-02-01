package services

import (
	"errors"
	"fmt"

	"vadimgribanov.com/tg-gpt/internal/config"
)

type LLMClientFactory struct {
	supportedModels map[string]config.LLMModel
	providers       map[string]LLMClient
}

func NewLLMClientFactory() *LLMClientFactory {
	return &LLMClientFactory{supportedModels: make(map[string]config.LLMModel), providers: make(map[string]LLMClient)}
}

func (f *LLMClientFactory) IsClientRegistered(name string) bool {
	_, ok := f.supportedModels[name]
	return ok
}

func (f *LLMClientFactory) RegisterProvider(name string, client LLMClient) {
	f.providers[name] = client
}

func (f *LLMClientFactory) RegisterClientUsingConfig(modelConfig config.LLMModel) {
	f.supportedModels[modelConfig.ModelId] = modelConfig
}

func (f *LLMClientFactory) GetClient(modelId string) (LLMClient, error) {
	if _, ok := f.supportedModels[modelId]; !ok {
		return nil, errors.New(fmt.Sprintf("Client with modelId %s not found", modelId))
	}
	if _, ok := f.providers[f.supportedModels[modelId].Provider]; !ok {
		return nil, errors.New(fmt.Sprintf("Provider with name %s not found", f.supportedModels[modelId].Provider))
	}

	return f.providers[f.supportedModels[modelId].Provider], nil
}
