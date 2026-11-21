package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"focusflash/tool"
)

func TestStartValidatesAndRunsCallsInSequence(t *testing.T) {
	var executed []string
	orchestrator, err := tool.NewOrchestrator([]tool.Registration{{
		Definition: tool.Definition{Name: "registered"},
		Validate: func(args json.RawMessage) error {
			if string(args) == "{}" {
				return errors.New("value is required")
			}
			return nil
		},
		Run: func(_ context.Context, args json.RawMessage) (string, error) {
			executed = append(executed, string(args))
			if string(args) == `{"value":"fail"}` {
				return "", errors.New("operation failed")
			}
			return "ok", nil
		},
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	results := <-orchestrator.Start(context.Background(), []tool.Call{
		{ID: "1", Name: "unknown", Args: json.RawMessage(`{}`)},
		{ID: "2", Name: "registered", Args: json.RawMessage(`[]`)},
		{ID: "3", Name: "registered", Args: json.RawMessage(`{}`)},
		{ID: "4", Name: "registered", Args: json.RawMessage(`{"value":"start"}`)},
		{ID: "5", Name: "registered", Args: json.RawMessage(`{"value":"fail"}`)},
	})
	wantCalls := []string{`{"value":"start"}`, `{"value":"fail"}`}
	if !slices.Equal(executed, wantCalls) {
		t.Fatalf("executed calls = %q, want %q", executed, wantCalls)
	}
	if len(results) != 5 {
		t.Fatalf("results = %+v", results)
	}
	for i, want := range []string{"unknown tool", "JSON object", "value is required", "", "operation failed"} {
		result := results[i]
		if result.CallID != string(rune('1'+i)) || (want == "" && result.Error != "") || !strings.Contains(result.Error, want) {
			t.Errorf("result %d = %+v, want error containing %q", i, result, want)
		}
	}
	if results[3].Output != "ok" {
		t.Errorf("successful result = %+v", results[3])
	}
}

func TestStartRunsCallsInParallelWithinLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var mu sync.Mutex
	var running, peak int
	orchestrator, err := tool.NewOrchestrator([]tool.Registration{{
		Definition: tool.Definition{Name: "registered"},
		Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			mu.Lock()
			running++
			peak = max(peak, running)
			mu.Unlock()
			defer func() {
				mu.Lock()
				running--
				mu.Unlock()
			}()
			started <- struct{}{}
			select {
			case <-release:
				return "ok", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	calls := []tool.Call{
		{ID: "a", Name: "registered", Args: json.RawMessage(`{}`)},
		{ID: "b", Name: "registered", Args: json.RawMessage(`{}`)},
		{ID: "c", Name: "registered", Args: json.RawMessage(`{}`)},
		{ID: "d", Name: "registered", Args: json.RawMessage(`{}`)},
	}
	done := orchestrator.Start(ctx, calls)
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("two tool calls did not run concurrently")
		}
	}
	close(release)
	select {
	case results := <-done:
		if peak != 2 || len(results) != len(calls) {
			t.Fatalf("peak concurrency = %d, results = %+v", peak, results)
		}
		for i, result := range results {
			if result.CallID != calls[i].ID || result.Output != "ok" || result.Error != "" {
				t.Fatalf("result %d = %+v", i, result)
			}
		}
	case <-ctx.Done():
		t.Fatal("tool batch did not finish")
	}
}
