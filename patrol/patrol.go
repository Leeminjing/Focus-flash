// Package patrol runs a workspace conversation on top of independent Agent sessions.
package patrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"focusflash/agent"
	"focusflash/tool"
)

// Patrol owns the workspace input history and coordinates one round at a time.
type Patrol struct {
	model       agent.Model
	workModel   agent.Model
	workTools   *tool.Orchestrator
	set         Set
	recorder    Recorder
	observation json.RawMessage
	inputs      []string
	turn        chan struct{}
}

// Set contains every work session in the workspace.
type Set struct {
	sessions map[string]workSession
}

type workSession struct {
	agent   *agent.Agent
	handoff agent.Message
}

// Round groups the work Runs dispatched for one user input.
type Round struct {
	ID          int
	Reply       string
	StartedAt   time.Time
	FinishedAt  time.Time
	Runs        []RunResult
	Observation Observation
}

type RunResult struct {
	SessionID  string
	StartedAt  time.Time
	FinishedAt time.Time
	Err        error
}

// InstantMessage is a workspace progress update emitted while a round runs.
type InstantMessage struct {
	RoundID   int
	SessionID string
	Stage     string
	Text      string
}

// Observation is the workspace state published after recording a round.
type Observation struct {
	Sessions   []SessionObservation `json:"sessions"`
	UserInputs []string             `json:"user_inputs"`
}

type SessionObservation struct {
	SessionID     string            `json:"session_id"`
	LatestArchive []ArchivedMessage `json:"latest_archive"`
	WorkingMemory string            `json:"working_memory"`
}

type prefixedModel struct {
	agent.Model
	prefix agent.Message
}

func (m prefixedModel) Generate(ctx context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
	request.Messages = append([]agent.Message{m.prefix}, request.Messages...)
	return m.Model.Generate(ctx, request, output)
}

// New uses dependencies assembled by the host. The three models serve the
// Patrol conversation, the work sessions, and the Recorder, respectively.
func New(model, workModel, recorderModel agent.Model, builtins *tool.Orchestrator) (*Patrol, error) {
	if model == nil || workModel == nil || recorderModel == nil {
		return nil, errors.New("Patrol requires conversation, work and Recorder models")
	}
	p := &Patrol{
		model:       model,
		workModel:   workModel,
		set:         Set{sessions: make(map[string]workSession)},
		recorder:    Recorder{model: recorderModel, sessions: make(map[string]*sessionArchive)},
		observation: json.RawMessage(`{"sessions":[],"user_inputs":[]}`),
		turn:        make(chan struct{}, 1),
	}
	workTools, err := p.workOrchestrator(builtins)
	if err != nil {
		return nil, err
	}
	p.workTools = workTools
	return p, nil
}

// Run accepts one user input and waits for the round and its Observation.
// Concurrent calls wait until the current Observation is published. The optional
// listener receives serialized progress callbacks before Run returns.
func (p *Patrol) Run(ctx context.Context, input string, progress func(InstantMessage)) (Round, error) {
	if strings.TrimSpace(input) == "" {
		return Round{}, errors.New("user input is required")
	}
	select {
	case p.turn <- struct{}{}:
		defer func() { <-p.turn }()
	case <-ctx.Done():
		return Round{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return Round{}, err
	}
	if p.observation == nil {
		return Round{}, errors.New("workspace observation could not be published")
	}

	previous := p.observation
	p.observation = nil
	p.inputs = append(p.inputs, input)
	round := Round{ID: len(p.inputs)}
	dispatch := roundDispatch{roundID: round.ID, progress: progress}
	reply, patrolErr := p.runConversation(ctx, input, previous, &dispatch)
	round.Reply = reply
	runs, runErr := dispatch.wait()
	round.Runs = runs
	if len(runs) > 0 {
		round.StartedAt = runs[0].StartedAt
		round.FinishedAt = runs[len(runs)-1].FinishedAt
		dispatch.emit("", "recording", "Updating session archives and working memory.")
	}
	recordErr := p.recorder.recordRound(ctx, dispatch.runs)
	if recordErr != nil {
		return round, errors.Join(patrolErr, runErr, recordErr)
	}
	round.Observation = p.observe()
	published, err := json.Marshal(round.Observation)
	if err != nil {
		return round, errors.Join(patrolErr, runErr, err)
	}
	p.observation = published
	dispatch.emit("", "published", "Workspace observation is ready.")
	return round, errors.Join(patrolErr, runErr)
}

func (p *Patrol) runConversation(ctx context.Context, input string, observation json.RawMessage, dispatch *roundDispatch) (string, error) {
	tools, err := p.patrolOrchestrator(ctx, dispatch)
	if err != nil {
		return "", err
	}
	model := prefixedModel{Model: p.model, prefix: agent.Message{
		Role: agent.RoleUser,
		Text: "You are the workspace Patrol. Use the observation below to decide whether to create sessions, then send tailored tasks to the selected sessions. " +
			"Use create_context for distinct new work and send_message to start work in an existing or newly created session. " +
			"Finish creating sessions before sending any tasks. When a handoff draws on prior work, include the source session IDs. " +
			"Treat the observation as records of past work and answer the current user. Workspace observation:\n" + string(observation),
	}}
	conversation, err := agent.New(model, agent.Config{Tools: tools})
	if err != nil {
		return "", err
	}
	result, err := conversation.Run(ctx, input)
	for i := len(result.Messages) - 1; i >= 0; i-- {
		message := result.Messages[i]
		if message.Role == agent.RoleAssistant && len(message.ToolCalls) == 0 {
			return message.Text, err
		}
	}
	return "", err
}

func (p *Patrol) observe() Observation {
	ids := make([]string, 0, len(p.set.sessions))
	for id := range p.set.sessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	snapshot := Observation{
		Sessions:   make([]SessionObservation, 0, len(ids)),
		UserInputs: slices.Clone(p.inputs),
	}
	for _, id := range ids {
		snapshot.Sessions = append(snapshot.Sessions, p.recorder.observe(id))
	}
	return snapshot
}

type pendingRun struct {
	status  RunResult
	handoff agent.Message
	result  agent.Result
	done    chan struct{}
}

type roundDispatch struct {
	roundID  int
	runs     []*pendingRun
	progress func(InstantMessage)
	emitMu   sync.Mutex
}

func (d *roundDispatch) send(ctx context.Context, id string, session workSession, message string) {
	var previous <-chan struct{}
	if len(d.runs) > 0 {
		previous = d.runs[len(d.runs)-1].done
	}
	run := &pendingRun{status: RunResult{SessionID: id}, handoff: session.handoff, done: make(chan struct{})}
	d.runs = append(d.runs, run)
	task, _, _ := strings.Cut(strings.TrimSpace(message), "\n")
	if runes := []rune(task); len(runes) > 120 {
		task = string(runes[:120]) + "…"
	}
	d.emit(id, "dispatched", "Assigned "+task)

	// Serialize runs in the shared workspace. The caller's context outlives the
	// Patrol's tool batch, so accepted work survives the end of dispatch.
	go func() {
		defer close(run.done)
		if previous != nil {
			<-previous
		}
		run.status.StartedAt = time.Now()
		d.emit(id, "started", "Working on "+task)
		run.result, run.status.Err = session.agent.Run(ctx, message)
		run.status.FinishedAt = time.Now()
		d.emit(id, "finished", "Run ended for "+task)
	}()
}

func (d *roundDispatch) wait() ([]RunResult, error) {
	runs := make([]RunResult, 0, len(d.runs))
	var runErr error
	for _, run := range d.runs {
		<-run.done
		runs = append(runs, run.status)
		if run.status.Err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("run %s: %w", run.status.SessionID, run.status.Err))
		}
	}
	return runs, runErr
}

func (d *roundDispatch) emit(id, stage, message string) {
	if d.progress == nil {
		return
	}
	d.emitMu.Lock()
	defer d.emitMu.Unlock()
	d.progress(InstantMessage{RoundID: d.roundID, SessionID: id, Stage: stage, Text: message})
}
