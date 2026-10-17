package meat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func codexToolResponse(t *testing.T, id, name string, input any) []byte {
	t.Helper()
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(codexOutput{Content: []codexOutputBlock{{
		Type:          "tool_use",
		ID:            id,
		ToolName:      name,
		ToolInputJSON: string(inputJSON),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func clearCodexAPIKey(t *testing.T) {
	t.Helper()
	t.Setenv("CODEX_API_KEY", "")
}

func TestAbridge_CodexCLIEditPlanEndToEnd(t *testing.T) {
	const diff = "diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	call := 0
	model := &CodexCLIModel{
		Executable: "codex",
		run: func(
			ctx context.Context,
			executable string,
			model string,
			prompt string,
			schemaPath string,
			outputPath string,
			workDir string,
		) ([]byte, []byte, error) {
			call++
			if executable != "codex" || model != "" {
				t.Errorf("executable/model = %q/%q", executable, model)
			}
			if !strings.Contains(prompt, `"system":"You are a code-reading assistant`) || !strings.Contains(prompt, "1|diff --git") {
				t.Errorf("prompt does not contain the MEAT turn: %s", prompt)
			}
			if call == 2 && !strings.Contains(prompt, "outside the diff") {
				t.Errorf("second turn does not contain the invalid-plan result: %s", prompt)
			}

			input := map[string]any{
				"remove":  []any{},
				"replace": []any{map[string]any{"line": 99, "old": "new", "new": "..."}},
				"fold":    []any{},
				"summary": "Changes the value.",
			}
			if call == 2 {
				input = map[string]any{
					"remove":  []any{map[string]any{"start_line": 3, "end_line": 3}},
					"replace": []any{map[string]any{"line": 4, "old": "new", "new": "n..."}},
					"fold":    []any{},
					"summary": "Changes the value.",
				}
			}
			if err := os.WriteFile(outputPath, codexToolResponse(t, "submit-plan", "submit", input), 0o600); err != nil {
				t.Fatal(err)
			}
			stdout := []byte(`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":5}}` + "\n")
			return stdout, nil, nil
		},
	}

	result, err := Abridge(context.Background(), model, Request{UnifiedDiff: diff})
	if err != nil {
		t.Fatal(err)
	}
	if call != 2 {
		t.Fatalf("Codex calls = %d, want invalid plan plus corrected plan", call)
	}
	if strings.Contains(result.SmartDiff, "-old") || !strings.Contains(result.SmartDiff, "+n...") {
		t.Fatalf("derived reading diff =\n%s", result.SmartDiff)
	}
	if result.InputTokens != 20 || result.OutputTokens != 10 {
		t.Errorf("tokens = %d/%d, want 20/10", result.InputTokens, result.OutputTokens)
	}
}

func TestCodexCommandArgs(t *testing.T) {
	prefix := []string{
		"exec",
		"--ephemeral",
		"--sandbox", "read-only",
		"--skip-git-repo-check",
		"--ignore-user-config",
		"--config", `model_reasoning_effort="medium"`,
		"--ignore-rules",
		"--disable", "shell_tool",
		"--json",
		"--color", "never",
		"--output-schema", "/tmp/schema",
		"--output-last-message", "/tmp/output",
	}
	want := append(slices.Clone(prefix), "--model", "gpt-5.6-sol", "-")
	if got := codexCommandArgs("gpt-5.6-sol", "/tmp/schema", "/tmp/output"); !slices.Equal(got, want) {
		t.Errorf("codexCommandArgs() = %q, want %q", got, want)
	}
	want = append(slices.Clone(prefix), "-")
	if got := codexCommandArgs("", "/tmp/schema", "/tmp/output"); !slices.Equal(got, want) {
		t.Errorf("codexCommandArgs(default) = %q, want %q", got, want)
	}
}

func TestNewModelFromEnv_CodexUsesCLI(t *testing.T) {
	clearCodexAPIKey(t)
	oldLookPath := codexLookPath
	oldLoginStatus := codexLoginStatus
	codexLookPath = func(file string) (string, error) {
		if file != "codex" {
			t.Fatalf("LookPath(%q), want codex", file)
		}
		return "/usr/local/bin/codex", nil
	}
	codexLoginStatus = func(context.Context, string) ([]byte, error) {
		return []byte("warning: keychain fallback\nLogged in using ChatGPT\n"), nil
	}
	t.Cleanup(func() {
		codexLookPath = oldLookPath
		codexLoginStatus = oldLoginStatus
	})

	model, err := NewModelFromEnv(context.Background(), "codex/gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	codex, ok := model.(*CodexCLIModel)
	if !ok {
		t.Fatalf("model = %T, want *CodexCLIModel", model)
	}
	if codex.Executable != "/usr/local/bin/codex" || codex.Model != "gpt-5.6-sol" {
		t.Errorf("executable/model = %q/%q", codex.Executable, codex.Model)
	}
}

func TestResolveModel_CodexPinsBuiltInDefault(t *testing.T) {
	for _, selector := range []string{"codex", "codex/"} {
		if got := ResolveModel(selector); got != "codex/"+DefaultModel {
			t.Errorf("ResolveModel(%q) = %q, want codex/%s", selector, got, DefaultModel)
		}
	}
}

func TestCodexModelName_DefaultSelectors(t *testing.T) {
	for _, selector := range []string{"codex", "codex/"} {
		if got := codexModelName(selector); got != DefaultModel {
			t.Errorf("codexModelName(%q) = %q, want %q", selector, got, DefaultModel)
		}
	}
}

func TestCacheProtocolHashIncludesOnlyCodexAdapterSurface(t *testing.T) {
	base := RubricHash()
	if got := CacheProtocolHash(DefaultModel); got != base {
		t.Errorf("CacheProtocolHash(non-Codex) = %q, want RubricHash %q", got, base)
	}

	model := CodexModel + "/" + DefaultModel
	surface := codexProtocolSurface()
	codexHash := CacheProtocolHash(model)
	if codexHash == base {
		t.Fatal("Codex cache protocol hash does not include the adapter surface")
	}
	if got := cacheProtocolHash(model, surface+" changed"); got == codexHash {
		t.Fatal("changing the Codex adapter surface did not change its cache protocol hash")
	}
	if got := cacheProtocolHash(DefaultModel, surface+" changed"); got != base {
		t.Fatal("changing the Codex adapter surface invalidated a non-Codex cache")
	}
}

func TestNewCodexCLIFromEnv_MissingExecutable(t *testing.T) {
	clearCodexAPIKey(t)
	old := codexLookPath
	codexLookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { codexLookPath = old })

	_, err := NewCodexCLIFromEnv(context.Background(), "codex")
	if err == nil || !strings.Contains(err.Error(), "codex CLI not found") {
		t.Fatalf("error = %v, want installation guidance", err)
	}
}

func TestNewCodexCLIFromEnv_RequiresChatGPTLogin(t *testing.T) {
	clearCodexAPIKey(t)
	oldLookPath := codexLookPath
	oldLoginStatus := codexLoginStatus
	codexLookPath = func(string) (string, error) { return "/usr/local/bin/codex", nil }
	codexLoginStatus = func(context.Context, string) ([]byte, error) {
		return []byte("Logged in using an API key\n"), nil
	}
	t.Cleanup(func() {
		codexLookPath = oldLookPath
		codexLoginStatus = oldLoginStatus
	})

	_, err := NewCodexCLIFromEnv(context.Background(), "codex")
	if err == nil || !strings.Contains(err.Error(), "not logged in with ChatGPT") {
		t.Fatalf("error = %v, want ChatGPT login guidance", err)
	}
}

func TestNewCodexCLIFromEnv_PreservesLoginStatusError(t *testing.T) {
	clearCodexAPIKey(t)
	oldLookPath := codexLookPath
	oldLoginStatus := codexLoginStatus
	codexLookPath = func(string) (string, error) { return "/usr/local/bin/codex", nil }
	codexLoginStatus = func(context.Context, string) ([]byte, error) {
		return []byte("keychain unavailable\n"), errors.New("exit status 1")
	}
	t.Cleanup(func() {
		codexLookPath = oldLookPath
		codexLoginStatus = oldLoginStatus
	})

	_, err := NewCodexCLIFromEnv(context.Background(), "codex")
	if err == nil || !strings.Contains(err.Error(), "exit status 1") || !strings.Contains(err.Error(), "keychain unavailable") {
		t.Fatalf("error = %v, want original failure and CLI detail", err)
	}
}

func TestNewCodexCLIFromEnv_PreservesCanceledContext(t *testing.T) {
	clearCodexAPIKey(t)
	oldLookPath := codexLookPath
	oldLoginStatus := codexLoginStatus
	codexLookPath = func(string) (string, error) { return "/usr/local/bin/codex", nil }
	codexLoginStatus = func(context.Context, string) ([]byte, error) {
		return nil, errors.New("signal: killed")
	}
	t.Cleanup(func() {
		codexLookPath = oldLookPath
		codexLoginStatus = oldLoginStatus
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewCodexCLIFromEnv(ctx, "codex")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestNewCodexCLIFromEnv_RejectsAPIKeyOverride(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "not-a-real-key")
	oldLookPath := codexLookPath
	codexLookPath = func(string) (string, error) {
		t.Fatal("Codex lookup must not run when CODEX_API_KEY is set")
		return "", nil
	}
	t.Cleanup(func() { codexLookPath = oldLookPath })

	_, err := NewCodexCLIFromEnv(context.Background(), "codex")
	if err == nil || !strings.Contains(err.Error(), "CODEX_API_KEY is set") || !strings.Contains(err.Error(), "unset it") {
		t.Fatalf("error = %v, want API-key override guidance", err)
	}
}

func TestCodexCLIModelGenerate_RejectsLateAPIKeyOverride(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "not-a-real-key")
	model := &CodexCLIModel{
		Executable: "codex",
		run: func(context.Context, string, string, string, string, string, string) ([]byte, []byte, error) {
			t.Fatal("Codex runner must not run when CODEX_API_KEY is set")
			return nil, nil, nil
		},
	}

	_, err := model.Generate(context.Background(), "system", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "CODEX_API_KEY is set") {
		t.Fatalf("error = %v, want API-key override rejection", err)
	}
}

func TestCodexCLIEnvironment_RemovesAPIKeyOverride(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "not-a-real-key")
	t.Setenv("MEAT_CODEX_ENV_TEST", "kept")
	env := codexCLIEnvironment()
	if slices.Contains(env, "CODEX_API_KEY=not-a-real-key") {
		t.Fatal("Codex child environment contains CODEX_API_KEY")
	}
	if !slices.Contains(env, "MEAT_CODEX_ENV_TEST=kept") {
		t.Fatal("Codex child environment lost unrelated variables")
	}
}

func TestNewCodexCLIFromEnv_TimesOutLoginStatus(t *testing.T) {
	clearCodexAPIKey(t)
	oldLookPath := codexLookPath
	oldLoginStatus := codexLoginStatus
	oldTimeout := codexLoginStatusTimeout
	codexLookPath = func(string) (string, error) { return "/usr/local/bin/codex", nil }
	codexLoginStatusTimeout = 20 * time.Millisecond
	codexLoginStatus = func(ctx context.Context, _ string) ([]byte, error) {
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	}
	t.Cleanup(func() {
		codexLookPath = oldLookPath
		codexLoginStatus = oldLoginStatus
		codexLoginStatusTimeout = oldTimeout
	})

	_, err := NewCodexCLIFromEnv(context.Background(), "codex")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want login-status deadline", err)
	}
	if !strings.Contains(err.Error(), "exceeded 20ms") || !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("error = %v, want explicit timeout and CLI failure", err)
	}
}

func TestAbridge_CodexInternalDeadlinePreservesCause(t *testing.T) {
	oldBudget := codexAbridgeBudget
	codexAbridgeBudget = 20 * time.Millisecond
	t.Cleanup(func() { codexAbridgeBudget = oldBudget })

	model := &CodexCLIModel{
		Executable: "codex",
		run: func(
			ctx context.Context,
			_, _, _, _, _, _ string,
		) ([]byte, []byte, error) {
			<-ctx.Done()
			return nil, nil, errors.New("signal: killed")
		},
	}
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"

	_, err := Abridge(context.Background(), model, Request{UnifiedDiff: diff})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want preserved agent deadline", err)
	}
	if !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("error = %v, want underlying Codex process failure", err)
	}
	if !strings.Contains(err.Error(), "agent run exceeded 20ms") {
		t.Fatalf("error = %v, want explicit MEAT budget", err)
	}
}

func TestModelAbridgeBudget_CodexGetsExtendedBudget(t *testing.T) {
	if got := modelAbridgeBudget(&CodexCLIModel{}); got != defaultCodexBudget {
		t.Errorf("Codex budget = %s, want %s", got, defaultCodexBudget)
	}
	if got := modelAbridgeBudget(&scriptedModel{}); got != defaultBudget {
		t.Errorf("default budget = %s, want %s", got, defaultBudget)
	}
}

func TestDecodeCodexContent_RejectsUnknownTool(t *testing.T) {
	raw := codexToolResponse(t, "call-1", "unknown", map[string]any{})
	_, err := decodeCodexContent(raw, []Tool{{Name: "submit"}})
	if err == nil || !strings.Contains(err.Error(), "invalid tool call") {
		t.Fatalf("error = %v, want invalid tool call", err)
	}
}
