package patrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"focusflash/agent"
	"focusflash/tool"
)

func (p *Patrol) patrolOrchestrator(ctx context.Context, dispatch *roundDispatch) (*tool.Orchestrator, error) {
	return tool.NewOrchestrator([]tool.Registration{
		{
			Definition: tool.Definition{
				Name: "create_context",
				Description: "Create a work session with a single handoff message and return its session ID. " +
					"Include relevant background and source session IDs in the message. This tool does not start work.",
				Parameters: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","minLength":1}},"required":["message"],"additionalProperties":false}`),
			},
			Run: func(_ context.Context, data json.RawMessage) (string, error) {
				args, err := decodeArgs[struct {
					Message string `json:"message"`
				}](data)
				if err != nil {
					return "", err
				}
				if strings.TrimSpace(args.Message) == "" {
					return "", errors.New("handoff message is required")
				}
				if len(dispatch.runs) > 0 {
					return "", errors.New("create sessions before dispatching work")
				}
				handoff := agent.Message{Role: agent.RoleUser, Text: args.Message}
				session, err := agent.New(prefixedModel{Model: p.workModel, prefix: handoff}, agent.Config{Tools: p.workTools})
				if err != nil {
					return "", err
				}
				id := fmt.Sprintf("session-%d", len(p.set.sessions)+1)
				p.set.sessions[id] = workSession{agent: session, handoff: handoff}
				return id, nil
			},
		},
		{
			Definition: tool.Definition{
				Name:        "send_message",
				Description: "Send one task message to a work session. Returns when the message is dispatched.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string"},"message":{"type":"string","minLength":1}},"required":["session_id","message"],"additionalProperties":false}`),
			},
			Run: func(_ context.Context, data json.RawMessage) (string, error) {
				args, err := decodeArgs[struct {
					SessionID string `json:"session_id"`
					Message   string `json:"message"`
				}](data)
				if err != nil {
					return "", err
				}
				session, ok := p.set.sessions[args.SessionID]
				if !ok {
					return "", fmt.Errorf("unknown work session %q", args.SessionID)
				}
				if strings.TrimSpace(args.Message) == "" {
					return "", errors.New("task message is required")
				}
				dispatch.send(ctx, args.SessionID, session, args.Message)
				return "dispatched", nil
			},
		},
	}, 1)
}

func (p *Patrol) workOrchestrator(builtins *tool.Orchestrator) (*tool.Orchestrator, error) {
	registrations := []tool.Registration{
		{
			Definition: tool.Definition{
				Name:        "read_session_history",
				Description: "Read a page of archived messages and tool results from a workspace session. Use a source session ID from a handoff when details are missing.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":100}},"required":["session_id","offset","limit"],"additionalProperties":false}`),
			},
			Run: p.recorder.readHistory,
		},
	}
	if builtins != nil {
		for _, definition := range builtins.Definitions() {
			registrations = append(registrations, tool.Registration{
				Definition: definition,
				Run: func(ctx context.Context, data json.RawMessage) (string, error) {
					// The original orchestrator still owns built-in task resources and cleanup.
					result := (<-builtins.Start(ctx, []tool.Call{{Name: definition.Name, Args: data}}))[0]
					if result.Error != "" {
						return "", errors.New(result.Error)
					}
					return result.Output, nil
				},
			})
		}
	}
	return tool.NewOrchestrator(registrations, 1)
}

func decodeArgs[T any](data json.RawMessage) (T, error) {
	var args T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&args)
	return args, err
}
