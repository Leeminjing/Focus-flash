package tool

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type sandboxMode string

const (
	sandboxReadOnly       sandboxMode = "read-only"
	sandboxWorkspaceWrite sandboxMode = "workspace-write"
	sandboxFullAccess     sandboxMode = "danger-full-access"

	defaultYield = time.Second
	maxYieldMS   = 10_000
	maxOutput    = 128 * 1024
)

type builtinRunKey struct{}

// The orchestrator owns these resources until the Agent finishes the task.
type builtinRun struct {
	workspace     string
	mode          sandboxMode
	powershell    string
	shellSetupErr error
	grant         workspaceGrant
	sessions      map[string]*shellSession
}

type shellArgs struct {
	Command     string `json:"command"`
	Workdir     string `json:"workdir"`
	YieldTimeMS *int   `json:"yield_time_ms"`
}

type stdinArgs struct {
	SessionID   string `json:"session_id"`
	Chars       string `json:"chars"`
	CloseStdin  bool   `json:"close_stdin"`
	YieldTimeMS *int   `json:"yield_time_ms"`
}

type shellResult struct {
	Output    string `json:"output"`
	SessionID string `json:"session_id,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// NewBuiltinOrchestrator assembles the Windows tools for an Agent. A nil mode
// uses the host permissions. The Agent supplies the task lifetime in Start.
func NewBuiltinOrchestrator(workspace string, configuredMode *string) (*Orchestrator, error) {
	mode := sandboxFullAccess
	if configuredMode != nil {
		mode = sandboxMode(*configuredMode)
	}
	switch mode {
	case sandboxReadOnly, sandboxWorkspaceWrite, sandboxFullAccess:
	default:
		return nil, fmt.Errorf("invalid sandbox_mode %q", mode)
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("open workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace %q is not a directory", root)
	}
	powershell, shellSetupErr := trustedPowerShell(root)
	registrations := []Registration{
		{
			Definition: Definition{
				Name: "shell", Description: "Run PowerShell 7, including reading and writing files as the current sandbox permits. If it is still running, use the returned session_id with write_stdin.",
				Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"workdir":{"type":"string"},"yield_time_ms":{"type":"integer","minimum":0,"maximum":10000}},"required":["command"],"additionalProperties":false}`),
			},
			Run: runShell,
		},
		{
			Definition: Definition{
				Name: "write_stdin", Description: "Send text to a shell session or poll for new output. Pass close_stdin to send EOF. Sessions last through the current task.",
				Parameters: json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string"},"chars":{"type":"string"},"close_stdin":{"type":"boolean"},"yield_time_ms":{"type":"integer","minimum":0,"maximum":10000}},"required":["session_id"],"additionalProperties":false}`),
			},
			Run: runWriteStdin,
		},
		{
			Definition: Definition{
				Name: "edit", Description: "Replace one unique literal match in an existing UTF-8 file. Pass an empty new_text to delete the match.",
				Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_text":{"type":"string","minLength":1},"new_text":{"type":"string"}},"required":["path","old_text","new_text"],"additionalProperties":false}`),
			},
			Run: runEdit,
		},
	}
	orchestrator, err := NewOrchestrator(registrations, 1)
	if err != nil {
		return nil, err
	}
	// The slot address is unique to an Agent.Run. Start installs the cleanup
	// before dispatching any work, and Run joins all batches before invoking it.
	var mu sync.Mutex
	runs := make(map[*func() error]*builtinRun)
	orchestrator.batchContext = func(ctx context.Context) context.Context {
		slot, ok := ctx.Value(RunCleanupKey{}).(*func() error)
		if !ok || slot == nil {
			return ctx
		}
		mu.Lock()
		defer mu.Unlock()
		run := runs[slot]
		if run == nil {
			run = &builtinRun{
				workspace: root, mode: mode, powershell: powershell, shellSetupErr: shellSetupErr,
				sessions: make(map[string]*shellSession),
			}
			runs[slot] = run
			*slot = func() error {
				mu.Lock()
				delete(runs, slot)
				mu.Unlock()
				return finishBuiltinRun(run)
			}
		}
		return context.WithValue(ctx, builtinRunKey{}, run)
	}
	return orchestrator, nil
}

func finishBuiltinRun(run *builtinRun) error {
	var cleanupErr error
	for id, session := range run.sessions {
		if err := session.stop(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("stop shell session %s: %w", id, err))
		}
	}
	if cleanupErr == nil && run.grant.cleanup != nil {
		cleanupErr = errors.Join(cleanupErr, run.grant.cleanup())
	}
	return cleanupErr
}

func runShell(ctx context.Context, data json.RawMessage) (string, error) {
	args, err := decodeBuiltinArgs[shellArgs](data)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(args.Command) == "" {
		return "", errors.New("command is required")
	}
	yield, err := shellYield(args.YieldTimeMS)
	if err != nil {
		return "", err
	}
	run, err := builtinRunFrom(ctx)
	if err != nil {
		return "", err
	}
	if run.shellSetupErr != nil {
		return "", run.shellSetupErr
	}
	workdir, err := builtinPath(run, args.Workdir)
	if err != nil {
		return "", fmt.Errorf("workdir: %w", err)
	}
	if err := validateShellCommand(ctx, run.powershell, args.Command, run.workspace, workdir, run.mode); err != nil {
		return "", err
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return "", fmt.Errorf("allocate shell session ID: %w", err)
	}
	if err := prepareBuiltinRun(ctx, run); err != nil {
		return "", err
	}
	process, err := startWindowsProcess(run.powershell, string(run.mode), workdir, args.Command, run.grant.workspaceSID, run.grant.tempSID, run.grant.tempPath)
	if err != nil {
		return "", err
	}
	id := hex.EncodeToString(idBytes[:])
	session := newShellSession(process)
	run.sessions[id] = session
	return pollShell(ctx, run, session, id, yield)
}

func runWriteStdin(ctx context.Context, data json.RawMessage) (string, error) {
	args, err := decodeBuiltinArgs[stdinArgs](data)
	if err != nil {
		return "", err
	}
	yield, err := shellYield(args.YieldTimeMS)
	if err != nil {
		return "", err
	}
	run, err := builtinRunFrom(ctx)
	if err != nil {
		return "", err
	}
	session, ok := run.sessions[args.SessionID]
	if !ok {
		return "", errors.New("unknown shell session in this task")
	}
	if args.Chars != "" || args.CloseStdin {
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := session.send(writeCtx, args.Chars, args.CloseStdin); err != nil {
			return "", err
		}
	}
	return pollShell(ctx, run, session, args.SessionID, yield)
}

func runEdit(ctx context.Context, data json.RawMessage) (string, error) {
	args, err := decodeBuiltinArgs[struct {
		Path    string  `json:"path"`
		OldText string  `json:"old_text"`
		NewText *string `json:"new_text"`
	}](data)
	if err != nil {
		return "", err
	}
	if args.Path == "" || args.OldText == "" || args.NewText == nil {
		return "", errors.New("path, nonempty old_text, and new_text are required")
	}
	run, err := builtinRunFrom(ctx)
	if err != nil {
		return "", err
	}
	if run.mode == sandboxReadOnly {
		return "", errors.New("edit requires file write permission in the current sandbox mode")
	}
	path, err := builtinPath(run, args.Path)
	if err != nil {
		return "", err
	}
	if err := prepareBuiltinRun(ctx, run); err != nil {
		return "", err
	}
	return runWindowsEdit(ctx, editArgs{Path: path, OldText: args.OldText, NewText: *args.NewText}, run.mode, run.grant)
}

func builtinRunFrom(ctx context.Context) (*builtinRun, error) {
	run, ok := ctx.Value(builtinRunKey{}).(*builtinRun)
	if !ok {
		return nil, errors.New("built-in tools require an active Agent task")
	}
	return run, nil
}

func prepareBuiltinRun(ctx context.Context, run *builtinRun) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if run.mode == sandboxWorkspaceWrite && run.grant.cleanup == nil {
		grant, err := prepareWorkspace(ctx, run.workspace)
		if err != nil {
			return fmt.Errorf("prepare workspace-write sandbox: %w", err)
		}
		run.grant = grant
	}
	return ctx.Err()
}

func builtinPath(run *builtinRun, path string) (string, error) {
	if path == "" {
		return run.workspace, nil
	}
	if run.mode != sandboxFullAccess {
		return validateWorkspacePath(run.workspace, path)
	}
	if !filepath.IsAbs(path) && filepath.VolumeName(path) == "" && path[0] != '\\' && path[0] != '/' {
		return filepath.Join(run.workspace, path), nil
	}
	return filepath.Abs(path)
}

func shellYield(milliseconds *int) (time.Duration, error) {
	if milliseconds == nil {
		return defaultYield, nil
	}
	if *milliseconds < 0 || *milliseconds > maxYieldMS {
		return 0, fmt.Errorf("yield_time_ms must be between 0 and %d", maxYieldMS)
	}
	return time.Duration(*milliseconds) * time.Millisecond, nil
}

func pollShell(ctx context.Context, run *builtinRun, session *shellSession, id string, yield time.Duration) (string, error) {
	timer := time.NewTimer(yield)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-session.done:
	case <-timer.C:
	}
	result := shellResult{SessionID: id}
	select {
	case <-session.done:
		if session.process.treeStopped {
			delete(run.sessions, id)
		}
		if session.waitErr != nil {
			return "", session.waitErr
		}
		result.ExitCode = &session.exitCode
		result.SessionID = ""
	default:
	}
	result.Output, result.Truncated = session.output.take()
	encoded, err := json.Marshal(result)
	return string(encoded), err
}

func decodeBuiltinArgs[T any](data json.RawMessage) (T, error) {
	var args T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&args)
	return args, err
}
