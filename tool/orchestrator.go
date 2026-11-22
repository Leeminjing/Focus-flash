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
