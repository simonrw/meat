package meat

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The CLI turn protocol runs one MEAT model turn through a subscription CLI
// (Codex or Claude Code). The conversation is serialized into a single prompt
// and the CLI returns a schema-constrained reply that is decoded back into
// Blocks, so MEAT keeps control of tool execution and the edit-plan loop.

type cliTurn struct {
	System   string       `json:"system"`
	Messages []cliMessage `json:"messages"`
	Tools    []Tool       `json:"tools"`
}

type cliMessage struct {
	Role    Role       `json:"role"`
	Content []cliBlock `json:"content"`
}

type cliBlock struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ID         string          `json:"id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	ToolInput  json.RawMessage `json:"tool_input,omitempty"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
	ToolResult string          `json:"tool_result,omitempty"`
	ToolError  bool            `json:"tool_error,omitempty"`
}

const cliTurnInstruction = `Act as exactly one language-model turn for MEAT. Follow the system instruction and conversation in the JSON payload below. Do not execute the advertised tools yourself. To call a tool, return a tool_use block whose tool_input_json is valid JSON matching that tool's input schema. Otherwise return a text block. Return only the schema-constrained response.

MEAT turn payload:
`

func cliTurnPrompt(system string, messages []Message, tools []Tool) (string, error) {
	turn := cliTurn{System: system, Tools: tools}
	for _, message := range messages {
		converted := cliMessage{Role: message.Role}
		for _, block := range message.Content {
			if block.Type == "provider_state" {
				continue
			}
			converted.Content = append(converted.Content, cliBlock{
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
		return "", fmt.Errorf("encode CLI turn: %w", err)
	}
	return cliTurnInstruction + string(payload), nil
}

// cliTurnSurface renders the protocol's static wrapper, wire serialization and
// response schema for inclusion in the cache protocol hash.
func cliTurnSurface() string {
	prompt, err := cliTurnPrompt(
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
		panic("meat: CLI protocol fixture: " + err.Error())
	}
	return prompt + "\x00" + string(cliResponseSchema)
}

type cliOutput struct {
	Content []cliOutputBlock `json:"content"`
}

type cliOutputBlock struct {
	Type          string `json:"type"`
	Text          string `json:"text"`
	ID            string `json:"id"`
	ToolName      string `json:"tool_name"`
	ToolInputJSON string `json:"tool_input_json"`
}

func decodeCLIContent(raw []byte, tools []Tool) ([]Block, error) {
	var output cliOutput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&output); err != nil {
		return nil, fmt.Errorf("decode CLI response: %w", err)
	}
	if len(output.Content) == 0 {
		return nil, fmt.Errorf("decode CLI response: empty content")
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
				return nil, fmt.Errorf("decode CLI response: invalid tool call %q", block.ToolName)
			}
			input := json.RawMessage(block.ToolInputJSON)
			if !json.Valid(input) {
				return nil, fmt.Errorf("decode CLI response: invalid JSON for tool %q", block.ToolName)
			}
			content = append(content, Block{
				Type:      "tool_use",
				ID:        block.ID,
				ToolName:  block.ToolName,
				ToolInput: input,
			})
		default:
			return nil, fmt.Errorf("decode CLI response: unsupported block type %q", block.Type)
		}
	}
	return content, nil
}

func truncateCLIError(message string) string {
	const max = 4 * 1024
	if len(message) <= max {
		return message
	}
	return message[:max] + "... (truncated)"
}

var cliResponseSchema = []byte(`{
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
