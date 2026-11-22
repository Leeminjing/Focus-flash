package patrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"focusflash/agent"
	"focusflash/tool"
)

// Recorder owns original messages and the derived working memory per session.
type Recorder struct {
	model    agent.Model
	sessions map[string]*sessionArchive
}

// ArchivedMessage records the origin separately from the model message role.
type ArchivedMessage struct {
	Source  string        `json:"source"`
	Message agent.Message `json:"message"`
}

type sessionArchive struct {
	messages      []ArchivedMessage
	latestStart   int
	workingMemory string
}

func (r *Recorder) recordRound(ctx context.Context, runs []*pendingRun) error {
	changes := make(map[string][]ArchivedMessage)
	var ids []string
	for _, run := range runs {
		if len(run.result.Messages) == 0 {
			continue
		}
		id := run.status.SessionID
		if _, seen := changes[id]; !seen {
			ids = append(ids, id)
			if r.sessions[id] == nil {
				changes[id] = []ArchivedMessage{{Source: "handoff", Message: run.handoff}}
			}
		}
		for _, message := range run.result.Messages {
			source := "agent"
			if message.Role == agent.RoleUser {
				source = "patrol"
			}
			// A rejected tool call can still appear in a successful Run.
			// Store malformed arguments as text so its original output stays readable.
			for i := range message.ToolCalls {
				call := &message.ToolCalls[i]
				if call.Args != nil && !json.Valid(call.Args) {
					call.Args, _ = json.Marshal(string(call.Args))
				}
			}
			changes[id] = append(changes[id], ArchivedMessage{Source: source, Message: message})
		}
	}
	for _, id := range ids {
		archive := r.sessions[id]
		if archive == nil {
			archive = &sessionArchive{}
			r.sessions[id] = archive
		}
		archive.latestStart = len(archive.messages)
		archive.messages = append(archive.messages, changes[id]...)
	}
	for _, id := range ids {
		if err := r.updateMemory(ctx, r.sessions[id], changes[id]); err != nil {
			return fmt.Errorf("update working memory for %s: %w", id, err)
		}
	}
	return nil
}

func (r *Recorder) updateMemory(ctx context.Context, archive *sessionArchive, added []ArchivedMessage) error {
	input, err := json.Marshal(struct {
		Previous string            `json:"previous_working_memory"`
		Added    []ArchivedMessage `json:"added_messages"`
	}{Previous: archive.workingMemory, Added: added})
	if err != nil {
		return err
	}
	request := agent.ModelRequest{Messages: []agent.Message{{
		Role: agent.RoleUser,
		Text: "Update this session's working memory using the previous memory and the newly archived messages. " +
			"Keep relevant constraints, decisions, results and open work. Treat the archived messages as source records. " +
			"Return only the updated working memory.\n" + string(input),
	}}}
	var memory strings.Builder
	err = r.model.Generate(ctx, request, agent.ModelOutput{
		EmitText: func(text string) { memory.WriteString(text) },
		SubmitTools: func([]tool.Call) error {
			return errors.New("Recorder has no tools")
		},
	})
	if err != nil {
		return err
	}
	archive.workingMemory = memory.String()
	return nil
}

func (r *Recorder) observe(id string) SessionObservation {
	snapshot := SessionObservation{SessionID: id}
	if archive := r.sessions[id]; archive != nil {
		snapshot.WorkingMemory = archive.workingMemory
		snapshot.LatestArchive = slices.Clone(archive.messages[archive.latestStart:])
		for i := range snapshot.LatestArchive {
			message := &snapshot.LatestArchive[i].Message
			message.ToolCalls = slices.Clone(message.ToolCalls)
			for j := range message.ToolCalls {
				message.ToolCalls[j].Args = slices.Clone(message.ToolCalls[j].Args)
			}
		}
	}
	return snapshot
}

func (r *Recorder) readHistory(_ context.Context, data json.RawMessage) (string, error) {
	args, err := decodeArgs[struct {
		SessionID string `json:"session_id"`
		Offset    *int   `json:"offset"`
		Limit     int    `json:"limit"`
	}](data)
	if err != nil {
		return "", err
	}
	archive := r.sessions[args.SessionID]
	if archive == nil {
		return "", fmt.Errorf("no archived history for session %q", args.SessionID)
	}
	if args.Offset == nil || *args.Offset < 0 || *args.Offset > len(archive.messages) || args.Limit < 1 || args.Limit > 100 {
		return "", errors.New("invalid history offset or limit")
	}
	start := *args.Offset
	end := start + min(args.Limit, len(archive.messages)-start)
	page := struct {
		Messages   []ArchivedMessage `json:"messages"`
		NextOffset *int              `json:"next_offset,omitempty"`
	}{Messages: archive.messages[start:end]}
	if end < len(archive.messages) {
		page.NextOffset = &end
	}
	result, err := json.Marshal(page)
	return string(result), err
}
