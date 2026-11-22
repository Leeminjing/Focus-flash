//go:build windows

package app_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"focusflash/agent"
	"focusflash/app"
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
	nextPatrol := make(chan struct{}, 1)
	var workCalls atomic.Int32
	var sourceSession string

	runtime, err := app.NewPatrol(project, func(config modelConfig) (agent.Model, error) {
		if config.Name != "workspace-model" {
			return nil, fmt.Errorf("unexpected model config: %+v", config)
		}
		return patrolTestModel(func(ctx context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
			messages := request.Messages
			last := messages[len(messages)-1]
			switch {
			case hasTool(request, "create_context"):
				switch last.Text {
				case "first request":
					return output.SubmitTools([]tool.Call{{ID: "create", Name: "create_context", Args: []byte(`{"message":"handoff"}`)}})
				case "second request":
					nextPatrol <- struct{}{}
					if len(messages) != 2 || !strings.Contains(messages[0].Text, "first memory") {
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
						{ID: "read", Name: "send_message", Args: []byte(fmt.Sprintf(`{"session_id":%q,"message":"read previous"}`, last.Text))},
					})
				}
				for _, message := range messages {
					if message.ToolError != "" {
						return fmt.Errorf("Patrol tool failed: %s", message.ToolError)
					}
				}
				output.EmitText("dispatched")
			case hasTool(request, "read_session_history"):
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
				case last.Text == "read previous":
					return output.SubmitTools([]tool.Call{
						{ID: "history", Name: "read_session_history", Args: []byte(fmt.Sprintf(`{"session_id":%q,"offset":0,"limit":10}`, sourceSession))},
						{ID: "edit", Name: "edit", Args: []byte(`{"path":"from-patrol.txt","old_text":"before","new_text":"after"}`)},
					})
				case last.ToolCallID == "edit":
					history := messages[len(messages)-2]
					if last.ToolError != "" || history.ToolError != "" || !strings.Contains(history.Text, "original detail") || !strings.Contains(history.Text, "handoff") {
						return fmt.Errorf("work tools returned history %+v and edit %+v", history, last)
					}
					output.EmitText("read the original")
				default:
					return fmt.Errorf("unexpected worker input: %+v", last)
				}
			default:
				switch {
				case strings.Contains(last.Text, "updated detail"):
					if !strings.Contains(last.Text, "first memory") || strings.Contains(last.Text, "original detail") {
						return fmt.Errorf("Recorder did not receive only the delta and prior memory: %s", last.Text)
					}
					output.EmitText("updated memory")
				case strings.Contains(last.Text, "read the original"):
					output.EmitText("second context memory")
				case strings.Contains(last.Text, "original detail"):
					close(recording)
					select {
					case <-publish:
					case <-ctx.Done():
						return ctx.Err()
					}
					output.EmitText("first memory")
				default:
					return fmt.Errorf("unexpected Recorder input: %s", last.Text)
				}
			}
			return nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		reply      string
		memory     string
		inputs     []string
		executions int
		progress   []string
		err        error
	}
	run := func(input string) <-chan outcome {
		done := make(chan outcome, 1)
		go func() {
			var progress []string
			round, err := runtime.Run(ctx, input, func(update patrol.InstantMessage) {
				progress = append(progress, update.Stage)
			})
			result := outcome{
				reply: round.Reply, executions: len(round.Runs), progress: progress,
				inputs: round.Observation.UserInputs, err: err,
			}
			if len(round.Observation.Sessions) > 0 {
				result.memory = round.Observation.Sessions[0].WorkingMemory
			}
			done <- result
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
		t.Fatal("Patrol started before Recorder published work memory")
	case <-time.After(30 * time.Millisecond):
		close(publish)
	}
	wantRuns := []int{1, 2}
	wantMemory := []string{"first memory", "updated memory"}
	wantInputs := []string{"first request", "second request"}
	for i, result := range []outcome{<-first, <-second} {
		if result.err != nil || result.reply != "dispatched" || result.executions != wantRuns[i] ||
			result.memory != wantMemory[i] || !slices.Equal(result.inputs, wantInputs[:i+1]) {
			t.Fatalf("round %d returned %+v", i+1, result)
		}
		if !strings.Contains(strings.Join(result.progress, ","), "started") {
			t.Fatalf("round %d had no live Run progress: %v", i+1, result.progress)
		}
	}
	content, err := os.ReadFile(file)
	if err != nil || string(content) != "after" {
		t.Fatalf("built-in edit through the work session wrote %q, error %v", content, err)
	}
}

func hasTool(request agent.ModelRequest, name string) bool {
	for _, definition := range request.Tools {
		if definition.Name == name {
			return true
		}
	}
	return false
}
