package patrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"focusflash/agent"
	"focusflash/internal/agfs"
)

// ArchivedMessage records the origin separately from the model message role.
type ArchivedMessage struct {
	Source  string        `json:"source"`
	Message agent.Message `json:"message"`
}

func (round *Round) run(
	ctx context.Context, p *Patrol, set *Set, input string, observation json.RawMessage,
	dispatch *roundDispatch, storage *agfs.Storage, memory *memoryWorkerSignals,
) (runErr, finishErr error) {
	reply, patrolErr := p.runConversation(ctx, input, observation, set, dispatch)
	runs, workErr := dispatch.wait()
	round.Reply = reply
	round.Runs = runs
	if len(runs) > 0 {
		round.StartedAt = runs[0].StartedAt
		round.FinishedAt = runs[len(runs)-1].FinishedAt
		dispatch.emit("", "recording", "Updating session archives and working memory.")
	}
	// Preserve the accepted Runs' messages even if their caller canceled.
	finish := context.WithoutCancel(ctx)
	ids, err := round.archive(finish, storage, dispatch.runs)
	if err == nil {
		err = waitForMemory(finish, storage, memory, ids)
	}
	if err != nil {
		finishErr = fmt.Errorf("finish round %d: %w", round.ID, err)
	}
	return errors.Join(patrolErr, workErr), finishErr
}

func waitForMemory(ctx context.Context, storage *agfs.Storage, memory *memoryWorkerSignals, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	select {
	case memory.wake <- struct{}{}:
	default:
	}
	for {
		done, err := storage.Settled(ctx, ids)
		if done || err != nil {
			return err
		}
		select {
		case <-memory.settled:
		case <-memory.done:
			// A final settlement may have committed before the worker exited.
			done, err := storage.Settled(ctx, ids)
			if done || err != nil {
				return err
			}
			return memory.err
		}
	}
}

func (round *Round) archive(ctx context.Context, storage *agfs.Storage, runs []*pendingRun) ([]string, error) {
	var drafts []agfs.ArchiveDraft
	bySession := make(map[string]int)
	for _, run := range runs {
		if len(run.result.Messages) == 0 {
			continue
		}
		id := run.status.SessionID
		index, found := bySession[id]
		if !found {
			index = len(drafts)
			bySession[id] = index
			drafts = append(drafts, agfs.ArchiveDraft{SessionID: id})
			hasArchive, err := storage.HasArchive(ctx, id)
			if err != nil {
				return nil, err
			}
			if !hasArchive {
				if err := appendArchivedMessage(&drafts[index], "handoff", run.handoff); err != nil {
					return nil, err
				}
			}
		}
		for _, message := range run.result.Messages {
			source := "agent"
			if message.Role == agent.RoleUser {
				source = "patrol"
			}
			if err := appendArchivedMessage(&drafts[index], source, message); err != nil {
				return nil, err
			}
		}
	}
	return storage.CommitRound(ctx, round.commitID, drafts)
}

func appendArchivedMessage(draft *agfs.ArchiveDraft, source string, message agent.Message) error {
	// A recorded but rejected tool call may contain malformed JSON arguments.
	// Encode those arguments as a string to retain their original text.
	message.ToolCalls = slices.Clone(message.ToolCalls)
	for i := range message.ToolCalls {
		call := &message.ToolCalls[i]
		if call.Args != nil && !json.Valid(call.Args) {
			call.Args, _ = json.Marshal(string(call.Args))
		}
	}
	data, err := json.Marshal(ArchivedMessage{Source: source, Message: message})
	if err != nil {
		return fmt.Errorf("encode message for %s: %w", draft.SessionID, err)
	}
	draft.Data = append(draft.Data, data...)
	draft.Data = append(draft.Data, '\n')
	return nil
}

func readArchivedMessages(ctx context.Context, storage *agfs.Storage, path string) ([]ArchivedMessage, error) {
	data, err := storage.Read(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read archive %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var records []ArchivedMessage
	for {
		var record ArchivedMessage
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			return records, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode archive %s: %w", path, err)
		}
		records = append(records, record)
	}
}
