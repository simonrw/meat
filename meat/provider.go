package meat

import (
	"context"
	"os"
	"strings"
)

// DefaultModel is meat's built-in default model.
const DefaultModel = ClaudeModel

// ResolveModel applies the CLI model fallback chain: the explicit value, then
// $MEAT_MODEL, then DefaultModel. It performs no network or credential work, so
// callers can resolve the cache identity cheaply and offline.
func ResolveModel(model string) string {
	model = resolveModel(model, DefaultModel)
	if model == CodexModel || model == CodexModel+"/" {
		return CodexModel + "/" + DefaultOpenAIModel
	}
	if model == ClaudeModel || model == ClaudeModel+"/" {
		return ClaudeModel + "/" + DefaultClaudeCLIModel
	}
	return model
}

func resolveModel(model, fallback string) string {
	if model == "" {
		model = os.Getenv("MEAT_MODEL")
	}
	if model == "" {
		model = fallback
	}
	return model
}

// NewModelFromEnv constructs the built-in backend appropriate for model.
// "codex" selectors use the subscription-authenticated Codex CLI, "claude"
// selectors use the subscription-authenticated Claude Code CLI, Claude model
// IDs use Anthropic Messages, and all other IDs use OpenAI Responses.
func NewModelFromEnv(ctx context.Context, model string) (Model, error) {
	model = ResolveModel(model)
	if isCodexModel(model) {
		return NewCodexCLIFromEnv(ctx, model)
	}
	if isClaudeCLIModel(model) {
		return NewClaudeCLIFromEnv(ctx, model)
	}
	if isAnthropicModel(model) {
		return NewAnthropicFromEnv(ctx, model)
	}
	return NewOpenAIFromEnv(ctx, model)
}

func isCodexModel(model string) bool {
	return model == CodexModel || strings.HasPrefix(model, CodexModel+"/")
}

func isAnthropicModel(model string) bool {
	model = strings.TrimPrefix(model, "anthropic/")
	return strings.HasPrefix(model, "claude-")
}
