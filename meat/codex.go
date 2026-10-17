package meat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const CodexModel = "codex"

const defaultCodexLoginStatusTimeout = 10 * time.Second

var codexLoginStatusTimeout = defaultCodexLoginStatusTimeout

var codexLookPath = exec.LookPath

var codexLoginStatus = func(ctx context.Context, executable string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, executable, "login", "status")
	return cmd.CombinedOutput()
}

type codexRunner func(
	ctx context.Context,
	executable string,
	model string,
	prompt string,
	schemaPath string,
	outputPath string,
	workDir string,
) ([]byte, []byte, error)

// CodexCLIModel adapts a subscription-authenticated Codex CLI process to Model.
// Each Generate call is ephemeral and schema-constrained; MEAT remains in charge
// of the read-only repository tools and the multi-turn edit-plan protocol.
type CodexCLIModel struct {
	Executable string
	Model      string
	run        codexRunner
}

func NewCodexCLIFromEnv(ctx context.Context, model string) (*CodexCLIModel, error) {
	if err := rejectCodexAPIKeyOverride(); err != nil {
		return nil, err
	}
	executable, err := codexLookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("codex CLI not found in PATH: install it and run `codex login`")
	}
	statusCtx, cancel := context.WithTimeout(ctx, codexLoginStatusTimeout)
	defer cancel()
	status, err := codexLoginStatus(statusCtx, executable)
	if err != nil {
		if callerErr := ctx.Err(); callerErr != nil {
			return nil, fmt.Errorf("check Codex login status: %w (CLI error: %v)", callerErr, err)
		}
		if statusErr := statusCtx.Err(); statusErr != nil {
			return nil, fmt.Errorf("check Codex login status exceeded %s: %w (CLI error: %v)", codexLoginStatusTimeout, statusErr, err)
		}
		detail := strings.TrimSpace(string(status))
		if detail != "" {
			return nil, fmt.Errorf("check Codex login status: %w: %s", err, truncateCodexError(detail))
		}
		return nil, fmt.Errorf("check Codex login status: %w", err)
	}
	if !codexStatusUsesChatGPT(status) {
		return nil, fmt.Errorf("codex CLI is not logged in with ChatGPT: run `codex login`")
	}
	return &CodexCLIModel{
		Executable: executable,
		Model:      codexModelName(model),
		run:        runCodexCLI,
	}, nil
}

func (m *CodexCLIModel) Generate(ctx context.Context, system string, messages []Message, tools []Tool) (*Response, error) {
	if m.Executable == "" {
		return nil, fmt.Errorf("meat: CodexCLIModel needs an executable")
	}
	if err := rejectCodexAPIKeyOverride(); err != nil {
		return nil, err
	}

	prompt, err := codexPrompt(system, messages, tools)
	if err != nil {
		return nil, err
	}
	workDir, err := os.MkdirTemp("", "meat-codex-")
	if err != nil {
		return nil, fmt.Errorf("create Codex workspace: %w", err)
	}
	defer os.RemoveAll(workDir)

	schemaPath := filepath.Join(workDir, "response-schema.json")
	outputPath := filepath.Join(workDir, "response.json")
	if err := os.WriteFile(schemaPath, codexResponseSchema, 0o600); err != nil {
		return nil, fmt.Errorf("write Codex response schema: %w", err)
	}

	runner := m.run
	if runner == nil {
		runner = runCodexCLI
	}
	stdout, stderr, err := runner(ctx, m.Executable, m.Model, prompt, schemaPath, outputPath, workDir)
	if err != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = strings.TrimSpace(string(stdout))
		}
		if detail != "" {
			return nil, fmt.Errorf("codex CLI: %w: %s", err, truncateCodexError(detail))
		}
		return nil, fmt.Errorf("codex CLI: %w", err)
	}

	raw, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("read Codex response: %w", err)
	}
	content, err := decodeCodexContent(raw, tools)
	if err != nil {
		return nil, err
	}
	inputTokens, outputTokens, err := codexUsage(stdout)
	if err != nil {
		return nil, err
	}
	return &Response{Content: content, InputTokens: inputTokens, OutputTokens: outputTokens}, nil
}

func runCodexCLI(
	ctx context.Context,
	executable string,
	model string,
	prompt string,
	schemaPath string,
	outputPath string,
	workDir string,
) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, executable, codexCommandArgs(model, schemaPath, outputPath)...)
	cmd.Dir = workDir
	cmd.Env = codexCLIEnvironment()
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func rejectCodexAPIKeyOverride() error {
	if strings.TrimSpace(os.Getenv("CODEX_API_KEY")) != "" {
		return fmt.Errorf("CODEX_API_KEY is set; unset it to use the ChatGPT-authenticated Codex CLI")
	}
	return nil
}

func codexCLIEnvironment() []string {
	env := os.Environ()
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "CODEX_API_KEY=") {
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

func codexCommandArgs(model, schemaPath, outputPath string) []string {
	args := []string{
		"exec",
		"--ephemeral",
		"--sandbox", "read-only",
		"--skip-git-repo-check",
		"--ignore-user-config",
		"--config", `model_reasoning_effort="` + DefaultReasoningEffort + `"`,
		"--ignore-rules",
		"--disable", "shell_tool",
		"--json",
		"--color", "never",
		"--output-schema", schemaPath,
		"--output-last-message", outputPath,
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return append(args, "-")
}

func codexStatusUsesChatGPT(status []byte) bool {
	for _, line := range strings.Split(string(status), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), "Logged in using ChatGPT") {
			return true
		}
	}
	return false
}

func codexModelName(model string) string {
	model = strings.TrimSpace(model)
	if model == CodexModel || model == CodexModel+"/" {
		return DefaultModel
	}
	return strings.TrimPrefix(model, CodexModel+"/")
}

type codexTurn struct {
	System   string         `json:"system"`
	Messages []codexMessage `json:"messages"`
	Tools    []Tool         `json:"tools"`
}

type codexMessage struct {
	Role    Role         `json:"role"`
	Content []codexBlock `json:"content"`
}

type codexBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ID         string          `json:"id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolInput  json.RawMessage `json:"tool_input,omitempty"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
	ToolResult string          `json:"tool_result,omitempty"`
	ToolError  bool            `json:"tool_error,omitempty"`
}

const codexPromptInstruction = `Act as exactly one language-model turn for MEAT. Follow the system instruction and conversation in the JSON payload below. Do not execute the advertised tools yourself. To call a tool, return a tool_use block whose tool_input_json is valid JSON matching that tool's input schema. Otherwise return a text block. Return only the schema-constrained response.

MEAT turn payload:
`

func codexPrompt(system string, messages []Message, tools []Tool) (string, error) {
	turn := codexTurn{System: system, Tools: tools}
	for _, message := range messages {
		converted := codexMessage{Role: message.Role}
		for _, block := range message.Content {
			if block.Type == "provider_state" {
				continue
			}
			converted.Content = append(converted.Content, codexBlock{
				Type:       block.Type,
				Text:       block.Text,
				ID:         block.ID,
				ToolName:   block.ToolName,
				ToolInput:  block.ToolInput,
				ToolUseID:  block.ToolUseID,
				ToolResult: block.ToolResult,
				ToolError:  block.ToolError,
			})
		}
		turn.Messages = append(turn.Messages, converted)
	}
	payload, err := json.Marshal(turn)
	if err != nil {
		return "", fmt.Errorf("encode Codex turn: %w", err)
	}
	return codexPromptInstruction + string(payload), nil
}

func codexProtocolSurface() string {
	prompt, err := codexPrompt(
		"fixture system",
		[]Message{
			{Role: RoleUser, Content: []Block{textBlock("fixture user")}},
			{Role: RoleAssistant, Content: []Block{
				textBlock("fixture assistant"),
				{Type: "tool_use", ID: "fixture-call", ToolName: "fixture_tool", ToolInput: json.RawMessage(`{"path":"fixture.go"}`)},
				{Type: "provider_state", Provider: "fixture", ProviderData: json.RawMessage(`{"opaque":true}`)},
			}},
			{Role: RoleUser, Content: []Block{{Type: "tool_result", ToolUseID: "fixture-call", ToolResult: "fixture result", ToolError: true}}},
		},
		[]Tool{{Name: "fixture_tool", Description: "Fixture tool.", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	)
	if err != nil {
		panic("meat: codex protocol fixture: " + err.Error())
	}
	return prompt + "\x00" + string(codexResponseSchema)
}

type codexOutput struct {
	Content []codexOutputBlock `json:"content"`
}

type codexOutputBlock struct {
	Type          string `json:"type"`
	Text          string `json:"text"`
	ID            string `json:"id"`
	ToolName      string `json:"tool_name"`
	ToolInputJSON string `json:"tool_input_json"`
}

func decodeCodexContent(raw []byte, tools []Tool) ([]Block, error) {
	var output codexOutput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&output); err != nil {
		return nil, fmt.Errorf("decode Codex response: %w", err)
	}
	if len(output.Content) == 0 {
		return nil, fmt.Errorf("decode Codex response: empty content")
	}
	toolNames := make(map[string]bool, len(tools))
	for _, tool := range tools {
		toolNames[tool.Name] = true
	}
	content := make([]Block, 0, len(output.Content))
	for _, block := range output.Content {
		switch block.Type {
		case "text":
			content = append(content, textBlock(block.Text))
		case "tool_use":
			if block.ID == "" || !toolNames[block.ToolName] {
				return nil, fmt.Errorf("decode Codex response: invalid tool call %q", block.ToolName)
			}
			input := json.RawMessage(block.ToolInputJSON)
			if !json.Valid(input) {
				return nil, fmt.Errorf("decode Codex response: invalid JSON for tool %q", block.ToolName)
			}
			content = append(content, Block{
				Type:      "tool_use",
				ID:        block.ID,
				ToolName:  block.ToolName,
				ToolInput: input,
			})
		default:
			return nil, fmt.Errorf("decode Codex response: unsupported block type %q", block.Type)
		}
	}
	return content, nil
}

type codexEvent struct {
	Type  string `json:"type"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func codexUsage(stdout []byte) (int, int, error) {
	var inputTokens, outputTokens int
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var event codexEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return 0, 0, fmt.Errorf("decode Codex event: %w", err)
		}
		if event.Type == "turn.completed" {
			inputTokens = event.Usage.InputTokens
			outputTokens = event.Usage.OutputTokens
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, fmt.Errorf("read Codex events: %w", err)
	}
	if !found {
		return 0, 0, fmt.Errorf("decode Codex events: missing turn.completed usage")
	}
	return inputTokens, outputTokens, nil
}

func truncateCodexError(message string) string {
	const max = 4 * 1024
	if len(message) <= max {
		return message
	}
	return message[:max] + "... (truncated)"
}

var codexResponseSchema = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "content": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "type": {"type": "string", "enum": ["text", "tool_use"]},
          "text": {"type": "string"},
          "id": {"type": "string"},
          "tool_name": {"type": "string"},
          "tool_input_json": {"type": "string"}
        },
        "required": ["type", "text", "id", "tool_name", "tool_input_json"]
      }
    }
  },
  "required": ["content"]
}`)
