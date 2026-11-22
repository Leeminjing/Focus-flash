// Package agent runs a model against a conversation and two in-memory queues.
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"focusflash/tool"
)

type Config struct {
	Steering TakeMode
	FollowUp TakeMode
	Tools    *tool.Orchestrator
	OnEnd    func(EndEvent)
}

// Result contains the conversation entries recorded during the accepted run.
type Result struct {
	Messages []Message
}

type EndEvent struct {
	Result Result
	Err    error
}

// Agent owns one conversation and accepts one Run at a time.
type Agent struct {
	model Model
	tools *tool.Orchestrator
	onEnd func(EndEvent)

	mu       sync.Mutex
	running  bool
	steering messageQueue
	followUp messageQueue

	// Only the accepted Run may access the conversation.
	history []Message
}

func New(model Model, config Config) (*Agent, error) {
	if model == nil {
		return nil, errors.New("agent requires a model")
	}
	if config.Steering > TakeAll || config.FollowUp > TakeAll {
		return nil, errors.New("invalid message queue mode")
	}
	if config.Tools == nil {
		config.Tools = &tool.Orchestrator{}
	}
	return &Agent{
		model:    model,
		tools:    config.Tools,
		onEnd:    config.OnEnd,
		steering: messageQueue{mode: config.Steering},
		followUp: messageQueue{mode: config.FollowUp},
	}, nil
}

// Steer queues a message for the next steering checkpoint.
func (a *Agent) Steer(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steering.push(text)
}

// FollowUp queues a message for when the current work is ready to stop.
func (a *Agent) FollowUp(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.followUp.push(text)
}

// Run starts a task and waits for model processing and the completion listener.
// Steer and FollowUp can be called while Run is waiting.
func (a *Agent) Run(ctx context.Context, task string) (Result, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return Result{}, errors.New("agent is already running")
	}
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return Result{}, err
	}
	a.running = true
	start := len(a.history)
	a.history = append(a.history, Message{Role: RoleUser, Text: task})
	inputs := a.steering.take()
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	}()

	var cleanupTools func() error
	runContext, cancel := context.WithCancel(ctx)
	runContext = context.WithValue(runContext, tool.RunCleanupKey{}, &cleanupTools)
	err := func() (err error) {
		defer func() {
			cancel()
			if cleanupTools != nil {
				if cleanupErr := cleanupTools(); cleanupErr != nil {
					err = errors.Join(err, fmt.Errorf("clean up tools: %w", cleanupErr))
				}
			}
		}()
		return a.run(runContext, inputs)
	}()
	result := Result{Messages: cloneMessages(a.history[start:])}
	if a.onEnd != nil {
		a.onEnd(EndEvent{Result: Result{Messages: cloneMessages(result.Messages)}, Err: err})
	}
	return result, err
}

func (a *Agent) run(ctx context.Context, inputs []string) error {
	for {
		for _, input := range inputs {
			a.history = append(a.history, Message{Role: RoleUser, Text: input})
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		hadTools, err := a.generate(ctx)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		inputs = a.nextInputs(hadTools)
		if len(inputs) == 0 && !hadTools {
			return nil
		}
	}
}

func (a *Agent) generate(ctx context.Context) (bool, error) {
	var text strings.Builder
	var calls []tool.Call
	var toolResults <-chan []tool.Result
	batchContext, cancelBatch := context.WithCancel(ctx)
	defer func() {
		cancelBatch()
		if toolResults != nil {
			// A panicking model adapter may have already submitted a batch.
			// Join it before the enclosing task releases its resources.
			<-toolResults
		}
	}()
	request := ModelRequest{Messages: cloneMessages(a.history), Tools: a.tools.Definitions()}
	output := ModelOutput{
		EmitText: func(chunk string) { text.WriteString(chunk) },
		SubmitTools: func(batch []tool.Call) error {
			if len(batch) == 0 {
				return nil
			}
			if toolResults != nil {
				return errors.New("model already submitted the round's tool calls")
			}
			calls = cloneCalls(batch)
			toolResults = a.tools.Start(batchContext, calls)
			return nil
		},
	}
	err := a.model.Generate(ctx, request, output)
	if toolResults != nil {
		// Keep the request and its results together, including on model failure.
		a.history = append(a.history, Message{Role: RoleAssistant, Text: text.String(), ToolCalls: calls})
		results := <-toolResults
		toolResults = nil
		for _, result := range results {
			a.history = append(a.history, Message{
				Role:       RoleTool,
				Text:       result.Output,
				ToolCallID: result.CallID,
				ToolName:   result.Name,
				ToolError:  result.Error,
			})
		}
	} else if err == nil && text.Len() > 0 {
		a.history = append(a.history, Message{Role: RoleAssistant, Text: text.String()})
	}
	if err != nil {
		return false, fmt.Errorf("generate model response: %w", err)
	}
	return len(calls) != 0, nil
}

func (a *Agent) nextInputs(hadTools bool) []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	inputs := a.steering.take()
	if hadTools || len(inputs) != 0 {
		return inputs
	}
	return a.followUp.take()
}
