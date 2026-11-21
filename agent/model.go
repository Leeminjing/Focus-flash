package agent

import (
	"context"
	"slices"

	"focusflash/tool"
)

// Model adapts a model backend to one round of the Agent's conversation.
// Generate must call output callbacks on the calling goroutine before returning.
type Model interface {
	Generate(ctx context.Context, request ModelRequest, output ModelOutput) error
}

type ModelRequest struct {
	Messages []Message
	Tools    []tool.Definition
}

type ModelOutput struct {
	EmitText func(chunk string)
	// SubmitTools hands the complete batch of tool calls directly to the
	// orchestrator. The adapter may submit one nonempty batch per round.
	SubmitTools func(calls []tool.Call) error
}

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role       Role
	Text       string
	ToolCalls  []tool.Call
	ToolCallID string
	ToolName   string
	ToolError  string
}

func cloneMessages(messages []Message) []Message {
	messages = slices.Clone(messages)
	for i := range messages {
		messages[i].ToolCalls = cloneCalls(messages[i].ToolCalls)
	}
	return messages
}

func cloneCalls(calls []tool.Call) []tool.Call {
	calls = slices.Clone(calls)
	for i := range calls {
		calls[i].Args = slices.Clone(calls[i].Args)
	}
	return calls
}
