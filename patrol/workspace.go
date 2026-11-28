package patrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"focusflash/agent"
	"focusflash/internal/agfs"
	"focusflash/tool"
)

// Workspace owns the session Set, user input history, and published Observation.
type Workspace struct {
	patrol      Patrol
	set         Set
	storage     *agfs.Storage
	inputs      []string
	observation json.RawMessage
	turn        chan struct{}
	closing     chan struct{}
	closeOnce   sync.Once
	closeErr    error

	memory     memoryWorkerSignals
	stopWorker context.CancelFunc
}

// New starts a Workspace using the recovered storage and the three models
// assembled by the host. The Workspace takes ownership of storage on success.
func New(model, workModel, memoryModel agent.Model, builtins *tool.Orchestrator, storage *agfs.Storage) (*Workspace, error) {
	if model == nil || workModel == nil || memoryModel == nil || storage == nil {
		return nil, errors.New("Patrol requires conversation, work and memory models and workspace storage")
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &Workspace{
		patrol:      Patrol{model: model, workModel: workModel, workTools: builtins},
		set:         Set{sessions: make(map[string]workSession)},
		storage:     storage,
		observation: json.RawMessage(`{"sessions":[],"user_inputs":[]}`),
		turn:        make(chan struct{}, 1),
		closing:     make(chan struct{}),
		stopWorker:  cancel,
		memory: memoryWorkerSignals{
			wake:    make(chan struct{}, 1),
			settled: make(chan struct{}, 1),
			done:    make(chan struct{}),
		},
	}
	w.memory.wake <- struct{}{}
	go w.runMemoryWorker(ctx, memoryModel)
	return w, nil
}

// Run accepts a workspace input and waits for its Round and Observation.
// Concurrent calls wait for the current round. Progress callbacks are serialized.
func (w *Workspace) Run(ctx context.Context, input string, progress func(InstantMessage)) (Round, error) {
	if strings.TrimSpace(input) == "" {
		return Round{}, errors.New("user input is required")
	}
	select {
	case w.turn <- struct{}{}:
		defer func() { <-w.turn }()
	case <-w.closing:
		return Round{}, errors.New("workspace is closed")
	case <-ctx.Done():
		return Round{}, ctx.Err()
	}
	select {
	case <-w.closing:
		return Round{}, errors.New("workspace is closed")
	default:
	}
	if err := ctx.Err(); err != nil {
		return Round{}, err
	}
	if w.observation == nil {
		return Round{}, errors.New("workspace observation could not be published")
	}

	commitID, err := newID()
	if err != nil {
		return Round{}, fmt.Errorf("assign round commit ID: %w", err)
	}
	previous := w.observation
	w.observation = nil
	w.inputs = append(w.inputs, input)
	round := Round{ID: len(w.inputs), commitID: commitID}
	dispatch := roundDispatch{roundID: round.ID, progress: progress}
	runErr, finishErr := round.run(ctx, &w.patrol, &w.set, input, previous, &dispatch, w.storage, &w.memory)
	if finishErr != nil {
		return round, errors.Join(runErr, finishErr)
	}
	round.Observation, err = w.observe(context.WithoutCancel(ctx))
	if err != nil {
		return round, errors.Join(runErr, fmt.Errorf("observe round %d: %w", round.ID, err))
	}
	w.observation, err = json.Marshal(round.Observation)
	if err != nil {
		return round, errors.Join(runErr, err)
	}
	dispatch.emit("", "published", "Workspace observation is ready.")
	return round, runErr
}

// Close stops accepting inputs, finishes the accepted round, then stops the
// worker and releases storage. Pending deliveries remain in SQLite.
func (w *Workspace) Close() error {
	w.closeOnce.Do(func() {
		close(w.closing)
		w.turn <- struct{}{}
		defer func() { <-w.turn }()
		w.stopWorker()
		<-w.memory.done
		w.closeErr = errors.Join(w.memory.err, w.storage.Close())
	})
	return w.closeErr
}

func (w *Workspace) observe(ctx context.Context) (Observation, error) {
	ids := make([]string, 0, len(w.set.sessions))
	for id := range w.set.sessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	snapshot := Observation{
		Sessions:   make([]SessionObservation, 0, len(ids)),
		UserInputs: slices.Clone(w.inputs),
	}
	for _, id := range ids {
		archive, memory, err := w.storage.Latest(ctx, id)
		if err != nil {
			return Observation{}, err
		}
		entry := SessionObservation{
			SessionID: id, ArchiveID: archive.ID, ArchiveSequence: archive.Sequence,
			ArchivePath: archive.FilePath, ArchiveState: archive.State,
			WorkingMemory: memory.L1, MemoryCoveredThrough: memory.CoveredThrough,
		}
		if archive.ID != "" {
			entry.LatestArchive, err = readArchivedMessages(ctx, w.storage, archive.Path)
			if err != nil {
				return Observation{}, err
			}
		}
		snapshot.Sessions = append(snapshot.Sessions, entry)
	}
	return snapshot, nil
}
