package meat

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func claudeResultJSON(t *testing.T, structured []byte) []byte {
	t.Helper()
	result := map[string]any{
		"type":              "result",
		"subtype":           "success",
		"is_error":          false,
		"structured_output": json.RawMessage(structured),
		"usage": map[string]int{
			"input_tokens":                2,
			"cache_creation_input_tokens": 5,
			"cache_read_input_tokens":     3,
			"output_tokens":               5,
		},
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func stubClaudeAuth(t *testing.T, status string) {
	t.Helper()
	oldLookPath := claudeLookPath
	oldAuthStatus := claudeAuthStatus
	claudeLookPath = func(file string) (string, error) {
		if file != "claude" {
			t.Fatalf("LookPath(%q), want claude", file)
		}
		return "/usr/local/bin/claude", nil
	}
	claudeAuthStatus = func(context.Context, string) ([]byte, error) {
		return []byte(status), nil
	}
	t.Cleanup(func() {
		claudeLookPath = oldLookPath
		claudeAuthStatus = oldAuthStatus
	})
}

func TestAbridge_ClaudeCLIEditPlanEndToEnd(t *testing.T) {
	const diff = "diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	call := 0
	model := &ClaudeCLIModel{
		Executable: "claude",
		Model:      DefaultClaudeCLIModel,
		run: func(ctx context.Context, executable string, args []string, prompt string, workDir string) ([]byte, []byte, error) {
			call++
			if executable != "claude" || !slices.Contains(args, DefaultClaudeCLIModel) {
				t.Errorf("executable/args = %q/%q", executable, args)
			}
			if !strings.Contains(prompt, `"system":"You are a code-reading assistant`) || !strings.Contains(prompt, "1|diff --git") {
				t.Errorf("prompt does not contain the MEAT turn: %s", prompt)
			}

			input := map[string]any{
				"remove":  []any{map[string]any{"start_line": 3, "end_line": 3}},
				"replace": []any{map[string]any{"line": 4, "old": "new", "new": "n..."}},
				"fold":    []any{},
				"summary": "Changes the value.",
			}
			return claudeResultJSON(t, codexToolResponse(t, "submit-plan", "submit", input)), nil, nil
		},
	}

	result, err := Abridge(context.Background(), model, Request{UnifiedDiff: diff})
	if err != nil {
		t.Fatal(err)
	}
	if call != 1 {
		t.Fatalf("Claude calls = %d, want 1", call)
	}

	if strings.Contains(result.SmartDiff, "-old") || !strings.Contains(result.SmartDiff, "+n...") {
		t.Fatalf("derived reading diff =\n%s", result.SmartDiff)
	}

	if result.InputTokens != 10 || result.OutputTokens != 5 {
		t.Errorf("tokens = %d/%d, want 10/5", result.InputTokens, result.OutputTokens)
	}
}

func TestClaudeCommandArgs(t *testing.T) {
	prefix := []string{
		"-p",
		"--output-format", "json",
		"--no-session-persistence",
		"--tools", "",
		"--strict-mcp-config",
		"--setting-sources", "",
		"--disable-slash-commands",
		"--effort", "high",
		"--system-prompt", claudeSystemPrompt,
		"--json-schema", string(cliResponseSchema),
	}

	want := append(slices.Clone(prefix), "--model", "claude-opus-5-5")
	if got := claudeCommandArgs("claude-opus-5-5"); !slices.Equal(got, want) {
		t.Errorf("claudeCommandArgs() = %q, want %q", got, want)
	}

	if got := claudeCommandArgs(""); !slices.Equal(got, prefix) {
		t.Errorf("claudeCommandArgs(default) = %q, want %q", got, prefix)
	}
}

func TestResolveModel_ClaudePinsOpus(t *testing.T) {
	for _, selector := range []string{"claude", "claude/"} {
		if got := ResolveModel(selector); got != "claude/claude-opus-5-5" {
			t.Errorf("ResolveModel(%q) = %q, want claude/claude-opus-5-5", selector, got)
		}
	}

	if got := ResolveModel("claude-sonnet-5-5"); got != "claude-sonnet-5-5" {
		t.Errorf("ResolveModel(API model) = %q, want it unchanged", got)
	}
}

func TestNewModelFromEnv_ClaudeUsesCLI(t *testing.T) {
	stubClaudeAuth(t, `{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}`)

	model, err := NewModelFromEnv(context.Background(), "claude/claude-sonnet-5-5")
	if err != nil {
		t.Fatal(err)
	}
	claude, ok := model.(*ClaudeCLIModel)
	if !ok {
		t.Fatalf("model = %T, want *ClaudeCLIModel", model)
	}
	if claude.Executable != "/usr/local/bin/claude" || claude.Model != "claude-sonnet-5-5" {
		t.Errorf("executable/model = %q/%q", claude.Executable, claude.Model)
	}

	t.Setenv("MEAT_MODEL", "")
	for _, selector := range []string{"", "claude"} {
		model, err = NewModelFromEnv(context.Background(), selector)
		if err != nil {
			t.Fatal(err)
		}
		claude, ok := model.(*ClaudeCLIModel)
		if !ok {
			t.Fatalf("NewModelFromEnv(%q) = %T, want *ClaudeCLIModel", selector, model)
		}
		if claude.Model != DefaultClaudeCLIModel {
			t.Errorf("NewModelFromEnv(%q) model = %q, want %q", selector, claude.Model, DefaultClaudeCLIModel)
		}
	}
}

func TestNewClaudeCLIFromEnv_MissingExecutable(t *testing.T) {
	old := claudeLookPath
	claudeLookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { claudeLookPath = old })

	_, err := NewClaudeCLIFromEnv(context.Background(), "claude")
	if err == nil || !strings.Contains(err.Error(), "claude CLI not found") {
		t.Fatalf("error = %v, want missing CLI", err)
	}
}

func TestNewClaudeCLIFromEnv_RequiresSubscriptionLogin(t *testing.T) {
	for name, status := range map[string]string{
		"logged out": `{"loggedIn":false}`,
		"api key":    `{"loggedIn":true,"authMethod":"api_key"}`,
	} {
		t.Run(name, func(t *testing.T) {
			stubClaudeAuth(t, status)

			_, err := NewClaudeCLIFromEnv(context.Background(), "claude")
			if err == nil || !strings.Contains(err.Error(), "not logged in with a Claude subscription") {
				t.Fatalf("error = %v, want subscription login error", err)
			}
		})
	}
}

func TestClaudeCLIEnvironment_RemovesAPIKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "token")
	t.Setenv("MEAT_UNRELATED", "kept")

	env := claudeCLIEnvironment()

	for _, entry := range env {
		if strings.HasPrefix(entry, "ANTHROPIC_API_KEY=") || strings.HasPrefix(entry, "ANTHROPIC_AUTH_TOKEN=") {
			t.Fatalf("Claude child environment contains %q", entry)
		}
	}
	if !slices.Contains(env, "MEAT_UNRELATED=kept") {
		t.Fatal("Claude child environment lost unrelated variables")
	}
}

func TestDecodeClaudeResult_ReportsErrorResult(t *testing.T) {
	raw := []byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"usage limit reached"}`)

	_, err := decodeClaudeResult(raw, nil)
	if err == nil || !strings.Contains(err.Error(), "usage limit reached") {
		t.Fatalf("error = %v, want CLI error result", err)
	}
}

func TestModelAbridgeBudget_ClaudeGetsExtendedBudget(t *testing.T) {
	if got := modelAbridgeBudget(&ClaudeCLIModel{}); got != defaultCLIBudget {
		t.Errorf("Claude budget = %s, want %s", got, defaultCLIBudget)
	}
}
