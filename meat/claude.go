package meat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ClaudeModel is the selector prefix for the subscription-authenticated
// Claude Code CLI: "claude" or "claude/<model>".
const ClaudeModel = "claude"

// DefaultClaudeCLIModel is the model used by the bare "claude" selector.
const DefaultClaudeCLIModel = "claude-opus-5-5"

// ClaudeCLIEffort is the effort level passed to every Claude Code CLI turn.
const ClaudeCLIEffort = "high"

// claudeSystemPrompt replaces Claude Code's default agent system prompt, which
// would otherwise steer the model towards acting as a coding assistant.
const claudeSystemPrompt = "You are a language-model backend for MEAT. Reply only with the schema-constrained structured output."

const claudeAuthStatusTimeout = 10 * time.Second

// claudeAPIKeyEnv are stripped from the child environment because Claude Code
// prefers them over the subscription login, and meat's Anthropic API backend
// may legitimately have them set.
var claudeAPIKeyEnv = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

var claudeLookPath = exec.LookPath

var claudeAuthStatus = func(ctx context.Context, executable string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, executable, "auth", "status", "--json")
	cmd.Env = claudeCLIEnvironment()
	return cmd.Output()
}

type claudeRunner func(ctx context.Context, executable string, args []string, prompt string, workDir string) ([]byte, []byte, error)

// ClaudeCLIModel adapts a subscription-authenticated Claude Code CLI process
// (`claude -p`) to Model. Each Generate call is a fresh, tool-less,
// schema-constrained session; MEAT remains in charge of the repository tools
// and the multi-turn edit-plan protocol.
type ClaudeCLIModel struct {
	Executable string
	Model      string
	run        claudeRunner
}

func NewClaudeCLIFromEnv(ctx context.Context, model string) (*ClaudeCLIModel, error) {
	executable, err := claudeLookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("claude CLI not found in PATH: install Claude Code and run `claude auth login`")
	}

	statusCtx, cancel := context.WithTimeout(ctx, claudeAuthStatusTimeout)
	defer cancel()
	raw, err := claudeAuthStatus(statusCtx, executable)
	if err != nil {
		if callerErr := ctx.Err(); callerErr != nil {
			return nil, fmt.Errorf("check Claude auth status: %w (CLI error: %v)", callerErr, err)
		}
		if statusErr := statusCtx.Err(); statusErr != nil {
			return nil, fmt.Errorf("check Claude auth status exceeded %s: %w (CLI error: %v)", claudeAuthStatusTimeout, statusErr, err)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if detail := strings.TrimSpace(string(exitErr.Stderr)); detail != "" {
				return nil, fmt.Errorf("check Claude auth status: %w: %s", err, truncateCLIError(detail))
			}
		}
		return nil, fmt.Errorf("check Claude auth status: %w", err)
	}

	var status struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, fmt.Errorf("decode Claude auth status: %w", err)
	}
	if !status.LoggedIn || status.AuthMethod != "claude.ai" {
		return nil, fmt.Errorf("claude CLI is not logged in with a Claude subscription: run `claude auth login`")
	}

	return &ClaudeCLIModel{
		Executable: executable,
		Model:      claudeModelName(model),
		run:        runClaudeCLI,
	}, nil
}

func (m *ClaudeCLIModel) Generate(ctx context.Context, system string, messages []Message, tools []Tool) (*Response, error) {
	if m.Executable == "" {
		return nil, fmt.Errorf("meat: ClaudeCLIModel needs an executable")
	}

	prompt, err := cliTurnPrompt(system, messages, tools)
	if err != nil {
		return nil, err
	}

	// An empty working directory keeps the CLI from discovering project
	// CLAUDE.md files or settings from wherever meat was invoked.
	workDir, err := os.MkdirTemp("", "meat-claude-")
	if err != nil {
		return nil, fmt.Errorf("create Claude workspace: %w", err)
	}
	defer os.RemoveAll(workDir)

	runner := m.run
	if runner == nil {
		runner = runClaudeCLI
	}
	stdout, stderr, err := runner(ctx, m.Executable, claudeCommandArgs(m.Model), prompt, workDir)
	if err != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = strings.TrimSpace(string(stdout))
		}
		if detail != "" {
			return nil, fmt.Errorf("claude CLI: %w: %s", err, truncateCLIError(detail))
		}
		return nil, fmt.Errorf("claude CLI: %w", err)
	}

	return decodeClaudeResult(stdout, tools)
}

func runClaudeCLI(ctx context.Context, executable string, args []string, prompt string, workDir string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = workDir
	cmd.Env = claudeCLIEnvironment()
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func claudeCLIEnvironment() []string {
	env := os.Environ()
	clean := make([]string, 0, len(env))
outer:
	for _, entry := range env {
		for _, name := range claudeAPIKeyEnv {
			if strings.HasPrefix(entry, name+"=") {
				continue outer
			}
		}
		clean = append(clean, entry)
	}
	return clean
}

// claudeCommandArgs builds a `claude -p` invocation with no tools, MCP
// servers, user settings, hooks, slash commands or session persistence.
func claudeCommandArgs(model string) []string {
	args := []string{
		"-p",
		"--output-format", "json",
		"--no-session-persistence",
		"--tools", "",
		"--strict-mcp-config",
		"--setting-sources", "",
		"--disable-slash-commands",
		"--effort", ClaudeCLIEffort,
		"--system-prompt", claudeSystemPrompt,
		"--json-schema", string(cliResponseSchema),
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

// claudeProtocolSurface renders everything the Claude adapter adds to the
// model-visible protocol, for the cache protocol hash.
func claudeProtocolSurface() string {
	return cliTurnSurface() + "\x00" + strings.Join(claudeCommandArgs(""), "\x00")
}

func isClaudeCLIModel(model string) bool {
	return model == ClaudeModel || strings.HasPrefix(model, ClaudeModel+"/")
}

func claudeModelName(model string) string {
	model = strings.TrimSpace(model)
	if model == ClaudeModel || model == ClaudeModel+"/" {
		return DefaultClaudeCLIModel
	}
	return strings.TrimPrefix(model, ClaudeModel+"/")
}

type claudeResult struct {
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Usage            struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
	} `json:"usage"`
}

func decodeClaudeResult(stdout []byte, tools []Tool) (*Response, error) {
	var result claudeResult
	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, fmt.Errorf("decode Claude result: %w", err)
	}
	if result.IsError || result.Subtype != "success" {
		return nil, fmt.Errorf("claude CLI: %s: %s", result.Subtype, truncateCLIError(result.Result))
	}
	if len(result.StructuredOutput) == 0 || string(result.StructuredOutput) == "null" {
		return nil, fmt.Errorf("decode Claude result: missing structured output")
	}

	content, err := decodeCLIContent(result.StructuredOutput, tools)
	if err != nil {
		return nil, err
	}

	usage := result.Usage
	return &Response{
		Content:      content,
		InputTokens:  usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens,
		OutputTokens: usage.OutputTokens,
	}, nil
}
