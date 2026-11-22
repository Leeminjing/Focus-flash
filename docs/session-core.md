# Session core

Focus-flash 的会话核心循环从 `Agent.Run` 接收任务开始，推进模型轮次和工具批次，按检查节点取用引导与后续消息，最后完成任务清理并返回会话记录。

## 变更审批

任何对 Focus-flash 核心循环代码的变更，都必须在实施前获得 **Leeminjing 的明确同意**。
本规定覆盖下列源码文件的所有改动，包括修复、重构、格式调整、移动和删除。涉及其他文件时，只要改动会改变核心循环的实现或行为，也须事先取得同意。
提交审批时说明涉及的文件、拟议改动与理由，并在当前会话或 PR 中记录 Leeminjing 的明确同意。改动范围超出已同意的内容时，须先取得对新增改动的同意。
经批准的源码变更须同步更新本文的目录和完整源码。

## 源码目录

源码行数包含注释和空行。

- [agent/agent.go](../agent/agent.go)（207 行）管理任务执行、会话历史、模型轮转和任务收尾。
- [agent/model.go](../agent/model.go)（59 行）定义单轮模型接口、会话消息和模型输出回调。
- [agent/queue.go](../agent/queue.go)（34 行）实现引导消息与后续消息队列。
- [tool/orchestrator.go](../tool/orchestrator.go)（125 行）调度工具调用批次，按调用顺序交付结果。
- [tool/tool.go](../tool/tool.go)（49 行）定义工具调用、结果与任务清理契约。

## 完整源码

### agent/agent.go

```go
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
```

### agent/model.go

```go
package agent

import (
	"context"
	"slices"

	"focusflash/tool"
)

// Model adapts a model backend to one round of the Agent's conversation.
// Generate must call output callbacks on the calling goroutine before returning.
type Model interface {
	Generate(ctx context.Context, request ModelRequest, output ModelOutput) error
}

type ModelRequest struct {
	Messages []Message
	Tools    []tool.Definition
}

type ModelOutput struct {
	EmitText func(chunk string)
	// SubmitTools hands the complete batch of tool calls directly to the
	// orchestrator. The adapter may submit one nonempty batch per round.
	SubmitTools func(calls []tool.Call) error
}

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role       Role
	Text       string
	ToolCalls  []tool.Call
	ToolCallID string
	ToolName   string
	ToolError  string
}

func cloneMessages(messages []Message) []Message {
	messages = slices.Clone(messages)
	for i := range messages {
		messages[i].ToolCalls = cloneCalls(messages[i].ToolCalls)
	}
	return messages
}

func cloneCalls(calls []tool.Call) []tool.Call {
	calls = slices.Clone(calls)
	for i := range calls {
		calls[i].Args = slices.Clone(calls[i].Args)
	}
	return calls
}
```

### agent/queue.go

```go
package agent

// TakeMode controls how many messages a queue yields at an execution checkpoint.
type TakeMode uint8

const (
	TakeOne TakeMode = iota
	TakeAll
)

// Each Agent owns a queue for each kind of input and serializes access to both.
type messageQueue struct {
	mode     TakeMode
	messages []string
}

func (q *messageQueue) push(text string) {
	q.messages = append(q.messages, text)
}

func (q *messageQueue) take() []string {
	if len(q.messages) == 0 {
		return nil
	}
	if q.mode == TakeAll || len(q.messages) == 1 {
		messages := q.messages
		q.messages = nil
		return messages
	}
	oldest := q.messages[0]
	q.messages[0] = ""
	q.messages = q.messages[1:]
	return []string{oldest}
}
```

### tool/orchestrator.go

```go
package tool

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
)

// Orchestrator owns a fixed tool registry and limits concurrency in each batch.
// Its zero value has no registered tools.
type Orchestrator struct {
	registry     map[string]Registration
	parallelism  int
	batchContext func(context.Context) context.Context
}

// NewOrchestrator registers tools. A parallelism of zero or one runs calls in order.
func NewOrchestrator(registrations []Registration, parallelism int) (*Orchestrator, error) {
	if parallelism < 0 {
		return nil, fmt.Errorf("negative tool parallelism: %d", parallelism)
	}
	registry := make(map[string]Registration, len(registrations))
	for _, registration := range registrations {
		name := registration.Definition.Name
		if name == "" || registration.Run == nil {
			return nil, fmt.Errorf("invalid registration for tool %q", name)
		}
		if _, exists := registry[name]; exists {
			return nil, fmt.Errorf("duplicate tool %q", name)
		}
		registration.Definition.Parameters = slices.Clone(registration.Definition.Parameters)
		registry[name] = registration
	}
	return &Orchestrator{registry: registry, parallelism: parallelism}, nil
}

func (o *Orchestrator) Definitions() []Definition {
	var definitions []Definition
	for _, registration := range o.registry {
		definition := registration.Definition
		definition.Parameters = slices.Clone(definition.Parameters)
		definitions = append(definitions, definition)
	}
	slices.SortFunc(definitions, func(a, b Definition) int { return cmp.Compare(a.Name, b.Name) })
	return definitions
}

// Start runs a batch asynchronously and returns one result per call in input order.
// Validation and execution failures are returned in the affected call's result.
func (o *Orchestrator) Start(ctx context.Context, calls []Call) <-chan []Result {
	if o.batchContext != nil && len(calls) > 0 {
		ctx = o.batchContext(ctx)
	}
	calls = slices.Clone(calls)
	for i := range calls {
		calls[i].Args = slices.Clone(calls[i].Args)
	}
	done := make(chan []Result, 1)
	go func() {
		defer close(done)
		done <- o.execute(ctx, calls)
	}()
	return done
}

func (o *Orchestrator) execute(ctx context.Context, calls []Call) []Result {
	results := make([]Result, len(calls))
	workers := min(max(1, o.parallelism), len(calls))
	if workers <= 1 {
		for i, call := range calls {
			results[i] = o.executeCall(ctx, call)
		}
		return results
	}

	jobs := make(chan int)
	var active sync.WaitGroup
	active.Add(workers)
	for range workers {
		go func() {
			defer active.Done()
			for i := range jobs {
				results[i] = o.executeCall(ctx, calls[i])
			}
		}()
	}
	for i := range calls {
		jobs <- i
	}
	close(jobs)
	active.Wait()
	return results
}

func (o *Orchestrator) executeCall(ctx context.Context, call Call) Result {
	result := Result{CallID: call.ID, Name: call.Name}
	registration, ok := o.registry[call.Name]
	switch {
	case !ok:
		result.Error = fmt.Sprintf("unknown tool %q", call.Name)
		return result
	case !validArguments(call.Args):
		result.Error = "tool arguments must be a JSON object"
		return result
	}
	if registration.Validate != nil {
		if err := registration.Validate(call.Args); err != nil {
			result.Error = fmt.Sprintf("invalid arguments for tool %q: %v", call.Name, err)
			return result
		}
	}
	if err := ctx.Err(); err != nil {
		result.Error = err.Error()
		return result
	}
	output, err := registration.Run(ctx, call.Args)
	if err != nil {
		result.Error = fmt.Sprintf("tool %q failed: %v", call.Name, err)
		return result
	}
	result.Output = output
	return result
}
```

### tool/tool.go

```go
// Package tool dispatches model calls to explicitly registered tools.
package tool

import (
	"bytes"
	"context"
	"encoding/json"
)

// Tool runs an operation with validated JSON object arguments.
// Implementations should honor cancellation through ctx.
type Tool func(ctx context.Context, args json.RawMessage) (string, error)

// RunCleanupKey identifies the task's cleanup slot (*func() error) in the
// context shared by all of its tool batches. The Agent owns the slot and
// invokes its contents after the last batch has returned.
type RunCleanupKey struct{}

// Definition describes a registered tool to a model adapter.
type Definition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

type Registration struct {
	Definition Definition
	// Validate can enforce tool-specific parameter constraints before Run.
	Validate func(args json.RawMessage) error
	Run      Tool
}

type Call struct {
	ID   string
	Name string
	Args json.RawMessage
}

type Result struct {
	CallID string
	Name   string
	Output string
	Error  string
}

func validArguments(args json.RawMessage) bool {
	trimmed := bytes.TrimSpace(args)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(args)
}
```
