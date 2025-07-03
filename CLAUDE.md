# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

This is a Go-based Telegram bot that provides an interface to multiple LLM providers (OpenAI and Anthropic). The bot supports text, voice, and image messages with streaming responses.

## Development Commands

### Building and Running
- `task build` - Build the binary (cross-compiled for Linux AMD64)
- `task deploy` - Deploy to remote server with backup
- `task rollback` - Rollback to previous version on remote server

### Testing
- `go test ./...` - Run all tests
- `go test ./internal/...` - Run tests for internal packages

### Code Quality
- `go fmt ./...` - Format code
- `go vet ./...` - Vet code for issues
- `go mod tidy` - Clean up dependencies

## Architecture

### Core Components

**Main Application Flow:**
1. `cmd/main/main.go` - Entry point, dependency injection, bot initialization
2. `internal/config/config.go` - YAML configuration loading
3. `internal/delivery/tgbot/handlers.go` - Telegram bot handlers and routing

**Service Layer:**
- `services/llm_client_proxy.go` - Multi-provider LLM client abstraction
- `services/text_service.go` - Text processing and conversation management
- `services/voice_service.go` - Voice transcription via OpenAI Whisper

**Adapter Pattern:**
- `adapters/openai.go` - OpenAI API adapter
- `adapters/anthropic.go` - Anthropic API adapter
- `adapters/streams.go` - Common streaming interface

**Data Layer:**
- `repositories/messages_repo.go` - Message storage and retrieval
- `repositories/user_repo.go` - User state management
- `models/` - Core data models (User, Interaction)

### Key Features

**Multi-Provider Support:**
- Configurable models via `config/application.yaml`
- Runtime model switching via `/change_model` command
- Unified streaming interface across providers

**Authentication & Rate Limiting:**
- User authentication via `ALLOWED_USER_ID` environment variable
- Concurrent request limiting via `middleware/rate_limiter.go`
- Per-user dialog timeout management

**Message Types:**
- Text messages with streaming responses
- Voice messages with transcription + LLM response
- Image messages with vision model support (requires caption)

## Configuration

### Environment Variables
- `TOKEN` - Telegram bot token
- `OPENAI_API_KEY` - OpenAI API key
- `ANTHROPIC_API_KEY` - Anthropic API key
- `ALLOWED_USER_ID` - Comma-separated list of allowed Telegram user IDs
- `REMOTE_HOST` - Remote server for deployment

### Application Config
Edit `config/application.yaml` to:
- Add/remove supported models
- Configure dialog timeout
- Set max concurrent requests per user

### Bot Commands
- `/start` - Welcome message
- `/new_chat` - Start new conversation context
- `/retry` - Retry last message
- `/current_model` - Show current model
- `/change_model` - Switch between available models
- `/cancel` - Cancel current request

## Database

Uses SQLite with in-memory storage for:
- User state (current model, dialog context)
- Message history and interactions
- Request tracking for rate limiting

## Deployment

The bot is designed for deployment to a remote Linux server using:
- Task runner for build/deploy automation
- Supervisor for process management
- SSH/rsync for file transfer
- Automatic backup/rollback capabilities