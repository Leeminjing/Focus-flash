package patrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"focusflash/agent"
	"focusflash/internal/agfs"
	"focusflash/internal/agfs/queuefs"
	"focusflash/tool"
)

type memoryWorkerSignals struct {
	wake    chan struct{}
	settled chan struct{}
	done    chan struct{}
	err     error // Read after done is closed.
}

func (w *Workspace) runMemoryWorker(ctx context.Context, model agent.Model) {
	defer close(w.memory.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.memory.wake:
		}
		for ctx.Err() == nil {
			data, err := w.storage.Read(ctx, agfs.WorkingMemoryDequeue)
			if err == nil && len(data) == 0 {
				break
			}
			if err == nil {
				err = processMemoryTask(ctx, model, w.storage, data)
			}
			if err != nil {
				if ctx.Err() == nil {
					w.memory.err = fmt.Errorf("working memory worker: %w", err)
				}
				return
			}
			select {
			case w.memory.settled <- struct{}{}:
			default:
			}
		}
	}
}

func processMemoryTask(ctx context.Context, model agent.Model, storage *agfs.Storage, data []byte) error {
	var message queuefs.Message
	if err := json.Unmarshal(data, &message); err != nil {
		return err
	}
	var task agfs.MemoryTask
	if err := json.Unmarshal(message.Data, &task); err != nil {
		return err
	}
	err := settleMemoryTask(ctx, model, storage, message.ID, task.ArchiveID)
	if err != nil {
		return fmt.Errorf("archive %s: %w", task.ArchiveID, err)
	}
	return nil
}

func settleMemoryTask(ctx context.Context, model agent.Model, storage *agfs.Storage, messageID int64, id string) error {
	baseline, archives, err := storage.MemoryInput(ctx, id)
	if err != nil {
		return err
	}
	var original []ArchivedMessage
	for _, archive := range archives {
		messages, err := readArchivedMessages(ctx, storage, archive.Path)
		if err != nil {
			return err
		}
		original = append(original, messages...)
	}
	target := archives[len(archives)-1]
	memory, err := generateWorkingMemory(ctx, model, baseline.L1, original, target.Sequence)
	if err != nil {
		return err
	}
	return storage.Settle(ctx, id, messageID, memory)
}

var memorySections = [...]string{
	"## Session Title",
	"## Current State",
	"## Task & Goals",
	"## Key Facts & Decisions",
	"## Files & Context",
	"## Errors & Corrections",
	"## Open Issues",
}

func generateWorkingMemory(ctx context.Context, model agent.Model, previous string, added []ArchivedMessage, coverage int) (agfs.WorkingMemory, error) {
	input, err := json.Marshal(struct {
		Previous string            `json:"previous_working_memory"`
		Added    []ArchivedMessage `json:"added_messages"`
	}{Previous: previous, Added: added})
	if err != nil {
		return agfs.WorkingMemory{}, err
	}
	request := agent.ModelRequest{Messages: []agent.Message{{
		Role: agent.RoleUser,
		Text: "Update the session's working memory from its previous version and the newly archived messages. " +
			"Treat archived messages as source records. Preserve facts, decisions and corrections that still apply. " +
			"Mark resolved issues as resolved. Use one plain-text summary sentence on the first line of Current State. " +
			"Return only Markdown under these seven headings, in order. Use None for an empty section.\n" +
			strings.Join(memorySections[:], "\n") + "\n\n" + string(input),
	}}}
	var output strings.Builder
	if err := model.Generate(ctx, request, agent.ModelOutput{
		EmitText: func(text string) { output.WriteString(text) },
		SubmitTools: func([]tool.Call) error {
			return errors.New("working memory generation has no tools")
		},
	}); err != nil {
		return agfs.WorkingMemory{}, fmt.Errorf("generate working memory: %w", err)
	}
	l1 := strings.TrimSpace(output.String())
	l0, err := validateWorkingMemory(l1)
	if err != nil {
		return agfs.WorkingMemory{}, err
	}
	return agfs.WorkingMemory{L0: l0, L1: l1, CoveredThrough: coverage}, nil
}

func validateWorkingMemory(l1 string) (string, error) {
	document := strings.TrimSpace(l1)
	if title, rest, _ := strings.Cut(document, "\n"); strings.TrimSpace(title) == "# Working Memory" {
		document = rest
	}
	section := -1
	hasContent := false
	var state string
	for _, line := range strings.Split(document, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "## ") {
			if section >= 0 && !hasContent {
				return "", fmt.Errorf("working memory section %q is empty", memorySections[section])
			}
			section++
			if section == len(memorySections) || line != memorySections[section] {
				return "", fmt.Errorf("unexpected working memory heading %q", line)
			}
			hasContent = false
			continue
		}
		if section < 0 {
			return "", errors.New("working memory must start with a section heading")
		}
		if section == 1 && !hasContent {
			state = line
		}
		hasContent = true
	}
	if section != len(memorySections)-1 || !hasContent {
		return "", errors.New("working memory must contain all seven nonempty sections")
	}
	return firstSentence(state), nil
}

func firstSentence(line string) string {
	line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
	for i, r := range line {
		switch r {
		case '。', '！', '？':
			return line[:i+utf8.RuneLen(r)]
		case '.', '!', '?':
			rest := line[i+1:]
			next, _ := utf8.DecodeRuneInString(rest)
			if rest == "" || unicode.IsSpace(next) {
				return line[:i+1]
			}
		}
	}
	return line
}
