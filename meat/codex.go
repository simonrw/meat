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
			return nil, fmt.Errorf("check Codex login status: %w: %s", err, truncateCLIError(detail))
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

	prompt, err := cliTurnPrompt(system, messages, tools)
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
	if err := os.WriteFile(schemaPath, cliResponseSchema, 0o600); err != nil {
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
			return nil, fmt.Errorf("codex CLI: %w: %s", err, truncateCLIError(detail))
		}
		return nil, fmt.Errorf("codex CLI: %w", err)
	}

	raw, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("read Codex response: %w", err)
	}
	content, err := decodeCLIContent(raw, tools)
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
		return DefaultOpenAIModel
	}
	return strings.TrimPrefix(model, CodexModel+"/")
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
