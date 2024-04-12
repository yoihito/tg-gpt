package services

import (
	"errors"
	"fmt"
)

var MODEL_MAP = map[string]string{
	"gpt-4-turbo-2024-04-09":   "openai",
	"gpt-4-turbo-preview":      "openai",
	"claude-3-opus-20240229":   "anthropic",
	"claude-3-sonnet-20240229": "anthropic",
	"claude-3-haiku-20240307":  "anthropic",
}

type LLMClientFactory struct {
	clients map[string]LLMClient
}

func NewLLMClientFactory() *LLMClientFactory {
	return &LLMClientFactory{clients: make(map[string]LLMClient)}
}

func (f *LLMClientFactory) IsClientRegistered(name string) bool {
	_, ok := f.clients[name]
	return ok
}

func (f *LLMClientFactory) RegisterClient(name string, client LLMClient) {
	f.clients[name] = client
}

func (f *LLMClientFactory) GetClient(name string) (LLMClient, error) {
	if _, ok := f.clients[name]; !ok {
		return nil, errors.New(fmt.Sprintf("Client with name %s not found", name))
	}
	return f.clients[name], nil
}
