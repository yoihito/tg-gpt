package services

import (
	"errors"
	"fmt"
)

type LLMClientFactory struct {
	clients map[string]LLMClient
}

func NewLLMClientFactory() *LLMClientFactory {
	return &LLMClientFactory{clients: make(map[string]LLMClient)}
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
