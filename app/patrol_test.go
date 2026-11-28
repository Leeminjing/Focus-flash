//go:build windows

package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"focusflash/agent"
	"focusflash/app"
	"focusflash/internal/agfs"
	"focusflash/patrol"
	"focusflash/tool"
)

type patrolTestModel func(context.Context, agent.ModelRequest, agent.ModelOutput) error

func (m patrolTestModel) Generate(ctx context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
	return m(ctx, request, output)
}

func TestPatrolRunsWorkspaceConversation(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "config.yaml"), []byte("model:\n  name: workspace-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(project, "from-patrol.txt")
	if err := os.WriteFile(file, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	recording := make(chan struct{})
	publish := make(chan struct{})
	finishing := make(chan struct{})
	finish := make(chan struct{})
	var publishOnce sync.Once
	var finishOnce sync.Once
	unblock := func() { publishOnce.Do(func() { close(publish) }) }
	finishRound := func() { finishOnce.Do(func() { close(finish) }) }
	nextPatrol := make(chan struct{}, 1)
	var workCalls atomic.Int32
	var sourceSession string

	runtime, err := app.NewPatrol(project, func(config modelConfig) (agent.Model, error) {
		if config.Name != "workspace-model" {
			return nil, fmt.Errorf("unexpected model config: %+v", config)
		}
		return patrolTestModel(func(_ context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
			messages := request.Messages
			last := messages[len(messages)-1]
			switch {
			case hasTool(request, "create_context"):
				switch last.Text {
				case "first request":
					return output.SubmitTools([]tool.Call{{ID: "create", Name: "create_context", Args: []byte(`{"message":"handoff"}`)}})
				case "second request":
					nextPatrol <- struct{}{}
					if len(messages) != 2 || !strings.Contains(messages[0].Text, "first memory") ||
						!strings.Contains(messages[0].Text, `"user_inputs":["first request"]`) {
						return fmt.Errorf("new Patrol session did not get the published Observation: %+v", messages)
					}
					return output.SubmitTools([]tool.Call{{
						ID: "second-context", Name: "create_context",
						Args: []byte(fmt.Sprintf(`{"message":%q}`, "look up "+sourceSession)),
					}})
				}
				switch last.ToolCallID {
				case "create":
					if last.ToolError != "" || workCalls.Load() != 0 {
						return fmt.Errorf("creation ran the worker or failed: %+v", last)
					}
					sourceSession = last.Text
					return output.SubmitTools([]tool.Call{{
						ID: "send", Name: "send_message", Args: []byte(fmt.Sprintf(`{"session_id":%q,"message":"first task"}`, sourceSession)),
					}})
				case "second-context":
					if last.ToolError != "" {
						return fmt.Errorf("second creation failed: %+v", last)
					}
					return output.SubmitTools([]tool.Call{
						{ID: "continue", Name: "send_message", Args: []byte(fmt.Sprintf(`{"session_id":%q,"message":"continue"}`, sourceSession))},
						{ID: "continue-again", Name: "send_message", Args: []byte(fmt.Sprintf(`{"session_id":%q,"message":"continue again"}`, sourceSession))},
						{ID: "edit", Name: "send_message", Args: []byte(fmt.Sprintf(`{"session_id":%q,"message":"edit file"}`, last.Text))},
					})
				}
				for _, message := range messages {
					if message.ToolError != "" {
						return fmt.Errorf("Patrol tool failed: %s", message.ToolError)
					}
				}
				output.EmitText("dispatched")
			case hasTool(request, "edit"):
				if hasTool(request, "read_session_history") {
					return fmt.Errorf("work session still received the retired history tool")
				}
				workCalls.Add(1)
				switch {
				case last.Text == "first task":
					if len(messages) != 2 || messages[0].Role != agent.RoleUser || messages[0].Text != "handoff" {
						return fmt.Errorf("first model request did not contain the handoff and task: %+v", messages)
					}
					output.EmitText("original detail")
				case last.Text == "continue":
					if len(messages) != 4 || messages[2].Text != "original detail" {
						return fmt.Errorf("existing worker lost its session: %+v", messages)
					}
					output.EmitText("updated detail")
				case last.Text == "continue again":
					if !strings.Contains(messages[len(messages)-2].Text, "updated detail") {
						return fmt.Errorf("earlier dispatch was not visible: %+v", messages)
					}
					output.EmitText("additional detail")
				case last.Text == "edit file":
					output.EmitText("editing the file")
					return output.SubmitTools([]tool.Call{
						{ID: "edit", Name: "edit", Args: []byte(`{"path":"from-patrol.txt","old_text":"before","new_text":"after"}`)},
					})
				case last.ToolCallID == "edit":
					if last.ToolError != "" {
						return fmt.Errorf("work tool returned %+v", last)
					}
					return fmt.Errorf("work model failed after the edit")
				default:
					return fmt.Errorf("unexpected worker input: %+v", last)
				}
			default:
				switch {
				case strings.Contains(last.Text, "updated detail"):
					if !strings.Contains(last.Text, "first memory") || strings.Contains(last.Text, "original detail") ||
						!strings.Contains(last.Text, "additional detail") {
						return fmt.Errorf("working memory did not receive the merged delta and its baseline: %s", last.Text)
					}
					output.EmitText(testWorkingMemory("updated memory"))
				case strings.Contains(last.Text, "editing the file"):
					close(finishing)
					select {
					case <-finish:
					case <-ctx.Done():
						return ctx.Err()
					}
					output.EmitText(testWorkingMemory("second context memory"))
				case strings.Contains(last.Text, "original detail"):
					close(recording)
					select {
					case <-publish:
					case <-ctx.Done():
						return ctx.Err()
					}
					output.EmitText(testWorkingMemory("first memory"))
				default:
					return fmt.Errorf("unexpected working memory input: %s", last.Text)
				}
			}
			return nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		unblock()
		finishRound()
		if err := runtime.Close(); err != nil {
			t.Error(err)
		}
	})

	type outcome struct {
		round    patrol.Round
		progress []string
		err      error
	}
	run := func(input string) <-chan outcome {
		done := make(chan outcome, 1)
		go func() {
			var progress []string
			round, err := runtime.Run(ctx, input, func(update patrol.InstantMessage) {
				progress = append(progress, update.Stage)
			})
			done <- outcome{round: round, progress: progress, err: err}
		}()
		return done
	}
	first := run("first request")
	select {
	case <-recording:
	case result := <-first:
		t.Fatalf("first round completed before recording: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := run("second request")
	select {
	case <-nextPatrol:
		t.Fatal("Patrol started before Workspace published the Observation")
	case <-time.After(30 * time.Millisecond):
		unblock()
	}
	select {
	case <-finishing:
	case result := <-second:
		t.Fatalf("second round returned before the last memory: %+v", result)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	closed := make(chan error, 1)
	go func() { closed <- runtime.Close() }()
	if _, err := runtime.Run(ctx, "input during close", nil); err == nil || !strings.Contains(err.Error(), "workspace is closed") {
		t.Fatalf("closing Workspace accepted input: %v", err)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before the accepted round finished: %v", err)
	default:
	}
	finishRound()
	wantRuns := []int{1, 3}
	wantMemory := []string{"first memory", "updated memory"}
	wantInputs := []string{"first request", "second request"}
	for i, result := range []outcome{<-first, <-second} {
		if (i == 0 && result.err != nil) || (i == 1 && (result.err == nil || !strings.Contains(result.err.Error(), "work model failed after the edit"))) {
			t.Fatalf("round %d error = %v", i+1, result.err)
		}
		round := result.round
		if round.Reply != "dispatched" || len(round.Runs) != wantRuns[i] ||
			!slices.Equal(round.Observation.UserInputs, wantInputs[:i+1]) {
			t.Fatalf("round %d returned %+v", i+1, result)
		}
		if len(round.Observation.Sessions) != i+1 {
			t.Fatalf("round %d sessions = %+v", i+1, round.Observation.Sessions)
		}
		for _, entry := range round.Observation.Sessions {
			records := readPatrolArchive(t, project, entry)
			if entry.ArchiveState != "complete" || entry.MemoryCoveredThrough != entry.ArchiveSequence {
				t.Fatalf("incomplete snapshot: %+v", entry)
			}
			if entry.SessionID != sourceSession {
				if len(records) != 4 || records[0].Source != "handoff" ||
					len(records[2].Message.ToolCalls) != 1 || records[2].Message.ToolCalls[0].Name != "edit" ||
					records[3].Message.ToolName != "edit" ||
					records[3].Message.ToolError != "" || records[3].Message.Text == "" {
					t.Fatalf("failed Run archive = %+v", records)
				}
				continue
			}
			if entry.ArchiveSequence != i+1 || entry.WorkingMemory != testWorkingMemory(wantMemory[i]) {
				t.Fatalf("unexpected snapshot: %+v", entry)
			}
			var texts, sources []string
			for _, record := range records {
				texts = append(texts, record.Message.Text)
				sources = append(sources, record.Source)
			}
			wantTexts := [][]string{{"handoff", "first task", "original detail"}, {"continue", "updated detail", "continue again", "additional detail"}}
			wantSources := [][]string{{"handoff", "patrol", "agent"}, {"patrol", "agent", "patrol", "agent"}}
			if !slices.Equal(texts, wantTexts[i]) || !slices.Equal(sources, wantSources[i]) {
				t.Fatalf("archive %d = %v with sources %v", i+1, texts, sources)
			}
		}
		if !strings.Contains(strings.Join(result.progress, ","), "started") {
			t.Fatalf("round %d had no live Run progress: %v", i+1, result.progress)
		}
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(file)
	if err != nil || string(content) != "after" {
		t.Fatalf("built-in edit through the work session wrote %q, error %v", content, err)
	}
}

func TestPatrolRestartsWithEmptySetAndContinuesMemory(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "config.yaml"), []byte("model:\n  name: workspace-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dbPath := filepath.Join(project, ".focus-flash", "patrol.sqlite3")
	store, err := agfs.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	oldID := "session-1"
	original := []byte("{\"source\":\"agent\",\"message\":{\"Role\":\"assistant\",\"Text\":\"recovered reply\"}}\n")
	ids, err := store.CommitRound(ctx, "previous-round", []agfs.ArchiveDraft{{SessionID: oldID, Data: original}})
	if err != nil {
		t.Fatal(err)
	}
	if data, err := store.Read(ctx, agfs.WorkingMemoryDequeue); err != nil || len(data) == 0 {
		t.Fatalf("could not simulate an interrupted delivery: %s, %v", data, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var newID string
	runtime, err := app.NewPatrol(project, func(modelConfig) (agent.Model, error) {
		return patrolTestModel(func(_ context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
			last := request.Messages[len(request.Messages)-1]
			switch {
			case hasTool(request, "create_context"):
				switch last.ToolCallID {
				case "":
					if !strings.Contains(request.Messages[0].Text, `{"sessions":[],"user_inputs":[]}`) {
						return fmt.Errorf("reopened Workspace did not start empty: %s", request.Messages[0].Text)
					}
					return output.SubmitTools([]tool.Call{{ID: "new", Name: "create_context", Args: []byte(`{"message":"new handoff"}`)}})
				case "new":
					if last.ToolError != "" || last.Text == oldID {
						return fmt.Errorf("could not create a distinct session: %+v", last)
					}
					newID = last.Text
					return output.SubmitTools([]tool.Call{
						{ID: "old", Name: "send_message", Args: []byte(fmt.Sprintf(`{"session_id":%q,"message":"old task"}`, oldID))},
						{ID: "live", Name: "send_message", Args: []byte(fmt.Sprintf(`{"session_id":%q,"message":"new task"}`, newID))},
					})
				case "live":
					rejected := request.Messages[len(request.Messages)-2]
					if !strings.Contains(rejected.ToolError, "unknown work session") || last.ToolError != "" {
						return fmt.Errorf("unexpected dispatch results: %+v, %+v", rejected, last)
					}
					output.EmitText("accepted new work")
				}
			case hasTool(request, "edit"):
				if last.Text != "new task" {
					return fmt.Errorf("unexpected work after restart: %+v", last)
				}
				output.EmitText("fresh reply")
			case strings.Contains(last.Text, "recovered reply"):
				output.EmitText(testWorkingMemory("Recovered. No second sentence in L0."))
			case strings.Contains(last.Text, "fresh reply"):
				output.EmitText(testWorkingMemory("Fresh."))
			default:
				return fmt.Errorf("unexpected memory input: %s", last.Text)
			}
			return nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	round, err := runtime.Run(ctx, "first input after restart", nil)
	if err != nil || len(round.Runs) != 1 || len(round.Observation.Sessions) != 1 ||
		round.Observation.Sessions[0].SessionID != newID || !slices.Equal(round.Observation.UserInputs, []string{"first input after restart"}) {
		t.Fatalf("round after restart = %+v, %v", round, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := agfs.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	archive, version, err := reopened.Latest(ctx, oldID)
	if err != nil || archive.ID != ids[0] || archive.State != "complete" ||
		version.CoveredThrough != 1 || version.L0 != "Recovered." || version.L1 != testWorkingMemory("Recovered. No second sentence in L0.") {
		t.Fatalf("old session did not settle after restart: %+v, %+v, %v", archive, version, err)
	}
	data, err := reopened.Read(ctx, archive.Path)
	if err != nil || !slices.Equal(data, original) {
		t.Fatalf("old archive changed after restart: %q, %v", data, err)
	}
}

func testWorkingMemory(state string) string {
	return "## Session Title\nExample session\n## Current State\n" + state +
		"\n## Task & Goals\nFinish the work\n## Key Facts & Decisions\nNone\n## Files & Context\nNone" +
		"\n## Errors & Corrections\nNone\n## Open Issues\nNone"
}

func readPatrolArchive(t *testing.T, project string, entry patrol.SessionObservation) []patrol.ArchivedMessage {
	t.Helper()
	want := filepath.Join(project, ".focus-flash", "sessions", entry.SessionID, entry.ArchiveID+".jsonl")
	if !filepath.IsAbs(entry.ArchivePath) || entry.ArchivePath != want {
		t.Fatalf("Observation archive_path = %q, want %q", entry.ArchivePath, want)
	}
	data, err := os.ReadFile(entry.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	var records []patrol.ArchivedMessage
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var message patrol.ArchivedMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("invalid JSONL archive line %q: %v", line, err)
		}
		records = append(records, message)
	}
	if len(records) != len(entry.LatestArchive) {
		t.Fatalf("Observation has %d messages, file has %d", len(entry.LatestArchive), len(records))
	}
	return records
}

func hasTool(request agent.ModelRequest, name string) bool {
	for _, definition := range request.Tools {
		if definition.Name == name {
			return true
		}
	}
	return false
}
