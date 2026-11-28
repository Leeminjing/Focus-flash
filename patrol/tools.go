package patrol

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"focusflash/agent"
	"focusflash/tool"
)

func (p *Patrol) patrolOrchestrator(ctx context.Context, set *Set, dispatch *roundDispatch) (*tool.Orchestrator, error) {
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
				id, err := newID()
				if err != nil {
					return "", err
				}
				id = "session-" + id
				set.sessions[id] = workSession{agent: session, handoff: handoff}
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
				session, ok := set.sessions[args.SessionID]
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

func newID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func decodeArgs[T any](data json.RawMessage) (T, error) {
	var args T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&args)
	return args, err
}
