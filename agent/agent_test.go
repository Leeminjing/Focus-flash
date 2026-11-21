package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"focusflash/agent"
	"focusflash/tool"
)

type modelFunc func(context.Context, agent.ModelRequest, agent.ModelOutput) error

func (fn modelFunc) Generate(ctx context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
	return fn(ctx, request, output)
}

func TestRunWaitsForToolsBeforeConsumingSteering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	modelWaiting := make(chan struct{})
	allowModel := make(chan struct{})
	toolStarted := make(chan struct{})
	releaseTool := make(chan struct{})
	orchestrator, err := tool.NewOrchestrator([]tool.Registration{{
		Definition: tool.Definition{Name: "lookup"},
		Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			close(toolStarted)
			select {
			case <-releaseTool:
				return "tool output", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	var roundInputs [][]agent.Message
	model := modelFunc(func(ctx context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
		roundInputs = append(roundInputs, request.Messages)
		if len(roundInputs) != 1 {
			output.EmitText("done")
			return nil
		}
		close(modelWaiting)
		select {
		case <-allowModel:
		case <-ctx.Done():
			return ctx.Err()
		}
		output.EmitText("looking up")
		return output.SubmitTools([]tool.Call{{ID: "call-1", Name: request.Tools[0].Name, Args: json.RawMessage(`{}`)}})
	})
	runner, err := agent.New(model, agent.Config{Tools: orchestrator})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, "task")
		done <- err
	}()
	await(t, ctx, modelWaiting)
	runner.Steer("during model")
	runner.FollowUp("next task")
	close(allowModel)
	await(t, ctx, toolStarted)
	runner.Steer("during tool")
	if _, err := runner.Run(ctx, "concurrent task"); err == nil {
		t.Fatal("concurrent task was accepted while a tool was running")
	}
	close(releaseTool)
	if err := await(t, ctx, done); err != nil {
		t.Fatal(err)
	}
	if len(roundInputs) != 4 {
		t.Fatalf("got %d model rounds, want 4", len(roundInputs))
	}
	second := roundInputs[1]
	if len(second) != 4 {
		t.Fatalf("second model request = %+v", second)
	}
	if reply := second[1]; reply.Text != "looking up" || len(reply.ToolCalls) != 1 || reply.ToolCalls[0].ID != "call-1" {
		t.Fatalf("assistant tool request = %+v", reply)
	}
	if result := second[2]; result.Role != agent.RoleTool || result.Text != "tool output" || result.ToolCallID != "call-1" {
		t.Fatalf("tool result = %+v", result)
	}
	for i, want := range []string{"task", "during model", "during tool", "next task"} {
		last := roundInputs[i][len(roundInputs[i])-1]
		if last.Role != agent.RoleUser || last.Text != want {
			t.Fatalf("round %d ended with %+v, want user message %q", i+1, last, want)
		}
	}
}

func TestRunWaitsForEndListenerAndKeepsLateMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listening := make(chan agent.EndEvent, 1)
	release := make(chan struct{})
	var rounds [][]agent.Message
	model := modelFunc(func(_ context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
		rounds = append(rounds, request.Messages)
		output.EmitText("complete")
		return nil
	})
	runner, err := agent.New(model, agent.Config{OnEnd: func(event agent.EndEvent) {
		listening <- event
		select {
		case <-release:
		case <-ctx.Done():
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, "first")
		done <- err
	}()
	event := await(t, ctx, listening)
	if event.Err != nil || len(event.Result.Messages) != 2 || event.Result.Messages[1].Text != "complete" {
		t.Fatalf("completion event = %+v", event)
	}
	select {
	case <-done:
		t.Fatal("Run returned before the listener finished")
	default:
	}
	runner.Steer("late steer")
	runner.FollowUp("late follow-up")
	if _, err := runner.Run(ctx, "concurrent"); err == nil {
		t.Fatal("accepted another task during completion")
	}
	close(release)
	if err := await(t, ctx, done); err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	var inputs []string
	for _, message := range result.Messages {
		if message.Role == agent.RoleUser {
			inputs = append(inputs, message.Text)
		}
	}
	if want := []string{"second", "late steer", "late follow-up"}; !slices.Equal(inputs, want) {
		t.Fatalf("second task inputs = %q, want %q", inputs, want)
	}
	if len(rounds) != 3 || rounds[1][0].Text != "first" {
		t.Fatalf("model did not continue the existing conversation: %+v", rounds)
	}
}

func TestRunKeepsToolResultsAndPendingMessagesAfterModelFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	generateErr := errors.New("model stream failed")
	toolDone := make(chan struct{})
	orchestrator, err := tool.NewOrchestrator([]tool.Registration{{
		Definition: tool.Definition{Name: "apply"},
		Run: func(context.Context, json.RawMessage) (string, error) {
			close(toolDone)
			return "applied", nil
		},
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var rounds [][]agent.Message
	model := modelFunc(func(_ context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
		rounds = append(rounds, request.Messages)
		if len(rounds) != 1 {
			output.EmitText("recovered")
			return nil
		}
		if err := output.SubmitTools([]tool.Call{
			{ID: "ok", Name: "apply", Args: json.RawMessage(`{}`)},
			{ID: "failed", Name: "missing", Args: json.RawMessage(`{}`)},
		}); err != nil {
			return err
		}
		select {
		case <-toolDone:
			return generateErr
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	var events []agent.EndEvent
	runner, err := agent.New(model, agent.Config{
		Tools: orchestrator,
		OnEnd: func(event agent.EndEvent) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.Steer("first steer")
	runner.Steer("still pending")
	runner.FollowUp("pending follow-up")
	failed, err := runner.Run(ctx, "first")
	if !errors.Is(err, generateErr) || len(events) != 1 || !errors.Is(events[0].Err, generateErr) {
		t.Fatalf("Run returned %v with events %+v", err, events)
	}
	messages := failed.Messages
	if len(messages) != 5 || messages[3].Text != "applied" || messages[4].ToolError == "" {
		t.Fatalf("failed run lost the tool responses: %+v", messages)
	}
	if _, err := runner.Run(ctx, "retry"); err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 3 {
		t.Fatalf("got %d model rounds, want 3", len(rounds))
	}
	retry := rounds[1]
	if len(retry) != 7 || retry[3].Text != "applied" || retry[5].Text != "retry" || retry[6].Text != "still pending" {
		t.Fatalf("retry did not see previous results and pending steering: %+v", retry)
	}
	followUp := rounds[2]
	if followUp[len(followUp)-1].Text != "pending follow-up" {
		t.Fatalf("retry lost the queued follow-up: %+v", followUp)
	}
}

func TestRunProcessesToolFailureBeforeFollowUp(t *testing.T) {
	var rounds [][]agent.Message
	model := modelFunc(func(_ context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
		rounds = append(rounds, request.Messages)
		if len(rounds) == 1 {
			return output.SubmitTools([]tool.Call{{ID: "bad-call", Name: "missing", Args: json.RawMessage(`{}`)}})
		}
		output.EmitText("continue")
		return nil
	})
	runner, err := agent.New(model, agent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runner.FollowUp("next task")
	if _, err := runner.Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 3 {
		t.Fatalf("got %d model rounds, want 3", len(rounds))
	}
	second := rounds[1][len(rounds[1])-1]
	if second.Role != agent.RoleTool || second.ToolCallID != "bad-call" || second.ToolError == "" {
		t.Fatalf("second round ended with %+v, want tool failure", second)
	}
	third := rounds[2][len(rounds[2])-1]
	if third.Role != agent.RoleUser || third.Text != "next task" {
		t.Fatalf("third round ended with %+v, want follow-up", third)
	}
}

func await[T any](t *testing.T, ctx context.Context, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		var zero T
		return zero
	}
}

func TestRunConsumesSteeringBeforeFollowUps(t *testing.T) {
	for _, tc := range []struct {
		name             string
		steering, follow agent.TakeMode
		want             []string
	}{
		{
			name: "default takes one from each queue",
			want: []string{"task, steer 1", "steer 2", "follow 1", "follow 2"},
		},
		{
			name:     "steering can take all independently",
			steering: agent.TakeAll,
			want:     []string{"task, steer 1, steer 2", "follow 1", "follow 2"},
		},
		{
			name:   "follow-up can take all independently",
			follow: agent.TakeAll,
			want:   []string{"task, steer 1", "steer 2", "follow 1, follow 2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rounds []string
			var seen int
			model := modelFunc(func(_ context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
				var inputs []string
				for _, message := range request.Messages[seen:] {
					if message.Role == agent.RoleUser {
						inputs = append(inputs, message.Text)
					}
				}
				seen = len(request.Messages)
				rounds = append(rounds, strings.Join(inputs, ", "))
				output.EmitText("re")
				output.EmitText("ply")
				return nil
			})
			runner, err := agent.New(model, agent.Config{Steering: tc.steering, FollowUp: tc.follow})
			if err != nil {
				t.Fatal(err)
			}
			runner.FollowUp("follow 1")
			runner.Steer("steer 1")
			runner.FollowUp("follow 2")
			runner.Steer("steer 2")
			result, err := runner.Run(context.Background(), "task")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(rounds, tc.want) {
				t.Fatalf("model rounds = %q, want %q", rounds, tc.want)
			}
			var replies []string
			for _, message := range result.Messages {
				if message.Role == agent.RoleAssistant {
					replies = append(replies, message.Text)
				}
			}
			if len(replies) != len(tc.want) {
				t.Fatalf("recorded replies = %q", replies)
			}
			for _, reply := range replies {
				if reply != "reply" {
					t.Fatalf("recorded reply = %q, want joined text chunks", reply)
				}
			}
		})
	}
}
