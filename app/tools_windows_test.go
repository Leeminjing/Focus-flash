package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"focusflash/agent"
	"focusflash/app"
	"focusflash/tool"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type modelFunc func(context.Context, agent.ModelRequest, agent.ModelOutput) error

func (m modelFunc) Generate(ctx context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
	return m(ctx, request, output)
}

func TestNewPublishesToolsWithDefaultHostPermissions(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	document := filepath.Join(workspace, "document.txt")
	if err := os.WriteFile(document, []byte("first\r\nold\r\nlast\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var called bool
	result := runToolModel(t, workspace, "", func(_ context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
		var names []string
		for _, definition := range request.Tools {
			names = append(names, definition.Name)
			if !json.Valid(definition.Parameters) {
				t.Fatalf("invalid JSON schema for %s", definition.Name)
			}
		}
		if !slices.Equal(names, []string{"edit", "shell", "write_stdin"}) {
			t.Fatalf("tool definitions = %v", names)
		}
		if called {
			return nil
		}
		called = true
		command := fmt.Sprintf("if ($PSVersionTable.PSVersion.Major -ne 7) { exit 49 }; [IO.File]::WriteAllText('%s', 'outside'); [Console]::WriteLine('command output'); exit 7", psQuote(outside))
		return output.SubmitTools([]tool.Call{
			toolCall(t, "shell", map[string]any{"command": command, "yield_time_ms": 10_000}),
			toolCall(t, "edit", map[string]any{"path": "document.txt", "old_text": "old\n", "new_text": "new\n"}),
		})
	})
	results := toolMessages(result)
	if len(results) != 2 || results[0].ToolError != "" || results[1].ToolError != "" {
		t.Fatalf("tool results = %+v", results)
	}
	shell := shellReply(t, results[0])
	if shell.ExitCode == nil || *shell.ExitCode != 7 || !strings.Contains(shell.Output, "command output") {
		t.Fatalf("shell = %+v", shell)
	}
	assertContents(t, outside, "outside")
	assertContents(t, document, "first\r\nnew\r\nlast\r\n")
}

func TestShellSessionInputAndTaskOwnership(t *testing.T) {
	workspace := t.TempDir()
	var activeID, expiredID string
	var seen strings.Builder
	step := 0
	model := modelFunc(func(_ context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
		step++
		switch step {
		case 1:
			return output.SubmitTools([]tool.Call{toolCall(t, "shell", map[string]any{
				"command":       "[Console]::WriteLine('ready'); $text = [Console]::ReadLine(); [Console]::WriteLine('echo:' + $text); exit 3",
				"yield_time_ms": 0,
			})})
		case 2:
			reply := shellReply(t, request.Messages[len(request.Messages)-1])
			activeID = reply.SessionID
			if activeID == "" || reply.ExitCode != nil {
				t.Fatalf("waiting shell returned %+v", reply)
			}
			seen.WriteString(reply.Output)
			return output.SubmitTools([]tool.Call{toolCall(t, "write_stdin", map[string]any{
				"session_id": activeID, "yield_time_ms": 0,
			})})
		case 3:
			reply := shellReply(t, request.Messages[len(request.Messages)-1])
			if reply.SessionID != activeID || reply.ExitCode != nil {
				t.Fatalf("poll returned %+v for waiting shell", reply)
			}
			seen.WriteString(reply.Output)
			return output.SubmitTools([]tool.Call{toolCall(t, "write_stdin", map[string]any{
				"session_id": activeID, "chars": "hello\n", "close_stdin": true, "yield_time_ms": 10_000,
			})})
		case 4:
			reply := shellReply(t, request.Messages[len(request.Messages)-1])
			seen.WriteString(reply.Output)
			if reply.SessionID != "" || reply.ExitCode == nil || *reply.ExitCode != 3 ||
				strings.Count(seen.String(), "ready") != 1 || strings.Count(seen.String(), "echo:hello") != 1 {
				t.Fatalf("shell finished with %+v and output %q", reply, seen.String())
			}
			return output.SubmitTools([]tool.Call{toolCall(t, "shell", map[string]any{
				"command": "[Console]::ReadLine()", "yield_time_ms": 0,
			})})
		case 5:
			expiredID = shellReply(t, request.Messages[len(request.Messages)-1]).SessionID
			if expiredID == "" || expiredID == activeID {
				t.Fatalf("invalid second shell ID %q", expiredID)
			}
		case 6:
			return output.SubmitTools([]tool.Call{toolCall(t, "write_stdin", map[string]any{"session_id": expiredID})})
		case 7:
			if last := request.Messages[len(request.Messages)-1]; !strings.Contains(last.ToolError, "unknown shell session in this task") {
				t.Fatalf("second task could use previous shell: %+v", last)
			}
		}
		return nil
	})
	runtime := toolRuntime(t, workspace, "", model)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, task := range []string{"first", "second"} {
		if _, err := runtime.Run(ctx, task); err != nil {
			t.Fatalf("task %s: %v", task, err)
		}
	}
	if step != 7 {
		t.Fatalf("generated %d rounds, want 7", step)
	}
}

func TestShellProcessIsStoppedAfterModelFailure(t *testing.T) {
	workspace := t.TempDir()
	var handle windows.Handle
	modelFailure := errors.New("model stopped before shell finished")
	var started bool
	runtime := toolRuntime(t, workspace, "", modelFunc(func(_ context.Context, request agent.ModelRequest, output agent.ModelOutput) error {
		if !started {
			started = true
			return output.SubmitTools([]tool.Call{toolCall(t, "shell", map[string]any{
				"command": "[Console]::WriteLine($PID); [Console]::ReadLine()", "yield_time_ms": 1_000,
			})})
		}
		reply := shellReply(t, request.Messages[len(request.Messages)-1])
		if reply.SessionID == "" {
			t.Fatalf("waiting shell returned %+v", reply)
		}
		if strings.TrimSpace(reply.Output) == "" {
			return output.SubmitTools([]tool.Call{toolCall(t, "write_stdin", map[string]any{
				"session_id": reply.SessionID, "yield_time_ms": 1_000,
			})})
		}
		pid, err := strconv.ParseUint(strings.TrimSpace(reply.Output), 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		handle, err = windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { windows.CloseHandle(handle) })
		return modelFailure
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := runtime.Run(ctx, "start a process and fail"); !errors.Is(err, modelFailure) {
		t.Fatalf("Run returned %v, want model failure", err)
	}
	status, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("process still running after task returned, wait status %d, error %v", status, err)
	}
}

func TestReadonlyBuiltinShellAndEdit(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	document := filepath.Join(workspace, "document.txt")
	if err := os.WriteFile(document, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	var called bool
	result := runToolModel(t, workspace, "read-only", func(_ context.Context, _ agent.ModelRequest, output agent.ModelOutput) error {
		if called {
			return nil
		}
		called = true
		return output.SubmitTools([]tool.Call{
			toolCall(t, "shell", map[string]any{"command": "[Console]::WriteLine([IO.File]::ReadAllText('document.txt')); try { [IO.File]::WriteAllText('document.txt', 'changed'); exit 0 } catch { exit 9 }", "yield_time_ms": 10_000}),
			toolCall(t, "edit", map[string]any{"path": "document.txt", "old_text": "original", "new_text": "changed"}),
			toolCall(t, "shell", map[string]any{"command": fmt.Sprintf("Set-Content -LiteralPath '%s' -Value 'outside'", psQuote(outside))}),
		})
	})
	results := toolMessages(result)
	if len(results) != 3 || !strings.Contains(results[1].ToolError, "requires file write permission") ||
		!strings.Contains(results[2].ToolError, "shell absolute path") {
		t.Fatalf("tool results = %+v", results)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("admission created an outside file: %v", err)
	}
	assertContents(t, document, "original")
	shell := shellReply(t, results[0])
	skipRestrictedPowerShell7(t, shell)
	if shell.ExitCode == nil || *shell.ExitCode != 9 || !strings.Contains(shell.Output, "original") {
		t.Fatalf("read-only shell = %+v", shell)
	}
}

func TestWorkspaceWriteBuiltinShellAndEdit(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	workspace := t.TempDir()
	document := filepath.Join(workspace, "document.txt")
	if err := os.WriteFile(document, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	var called bool
	result := runToolModel(t, workspace, "workspace-write", func(_ context.Context, _ agent.ModelRequest, output agent.ModelOutput) error {
		if called {
			return nil
		}
		called = true
		return output.SubmitTools([]tool.Call{
			toolCall(t, "shell", map[string]any{"command": "$ErrorActionPreference = 'Stop'; [IO.File]::WriteAllText('shell.txt', 'workspace'); [IO.File]::WriteAllText([IO.Path]::Combine($env:TEMP, 'temp.txt'), 'private'); [Console]::WriteLine($env:TEMP)", "yield_time_ms": 10_000}),
			toolCall(t, "edit", map[string]any{"path": "document.txt", "old_text": "original", "new_text": "changed"}),
		})
	})
	results := toolMessages(result)
	if len(results) != 2 || results[1].ToolError != "" {
		t.Fatalf("tool results = %+v", results)
	}
	assertContents(t, document, "changed")
	shell := shellReply(t, results[0])
	if shell.ExitCode != nil && *shell.ExitCode != 0 {
		if _, err := os.Stat(filepath.Join(workspace, "shell.txt")); !os.IsNotExist(err) {
			t.Fatalf("failed shell wrote a workspace file: %v", err)
		}
		skipRestrictedPowerShell7(t, shell)
	}
	privateTemp := strings.TrimSpace(shell.Output)
	if shell.ExitCode == nil || *shell.ExitCode != 0 || !filepath.IsAbs(privateTemp) {
		t.Fatalf("workspace-write shell = %+v", shell)
	}
	if _, err := os.Stat(privateTemp); !os.IsNotExist(err) {
		t.Fatalf("private temp remains after task: %q, stat error = %v", privateTemp, err)
	}
	assertContents(t, filepath.Join(workspace, "shell.txt"), "workspace")
}

func skipRestrictedPowerShell7(t *testing.T, result shellResponse) {
	t.Helper()
	if result.ExitCode == nil || *result.ExitCode != -532462766 || !strings.Contains(result.Output, "BCrypt.dll") ||
		!strings.Contains(result.Output, "0x8007045A") || windows.RtlGetVersion().BuildNumber != 26200 {
		return
	}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return
	}
	defer key.Close()
	if revision, _, err := key.GetIntegerValue("UBR"); err == nil && revision == 9974 {
		t.Skip("PowerShell 7 BCrypt initialization fails under this Windows build's restricted token")
	}
}

func runToolModel(t *testing.T, workspace, mode string, model modelFunc) agent.Result {
	t.Helper()
	runtime := toolRuntime(t, workspace, mode, model)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := runtime.Run(ctx, "exercise tools")
	if err != nil {
		t.Fatalf("Run: %v; messages: %+v", err, result.Messages)
	}
	return result
}

func toolRuntime(t *testing.T, workspace, mode string, model modelFunc) *agent.Agent {
	t.Helper()
	config := "model: {}\n"
	if mode != "" {
		config += "sandbox_mode: " + mode + "\n"
	}
	if err := os.WriteFile(filepath.Join(workspace, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	runtime, err := app.New(workspace, func(struct{}) (agent.Model, error) { return model, nil })
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func toolCall(t *testing.T, name string, args map[string]any) tool.Call {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Call{ID: name, Name: name, Args: encoded}
}

func toolMessages(result agent.Result) []agent.Message {
	var messages []agent.Message
	for _, message := range result.Messages {
		if message.Role == agent.RoleTool {
			messages = append(messages, message)
		}
	}
	return messages
}

type shellResponse struct {
	Output    string `json:"output"`
	SessionID string `json:"session_id"`
	ExitCode  *int   `json:"exit_code"`
}

func shellReply(t *testing.T, message agent.Message) shellResponse {
	t.Helper()
	if message.ToolError != "" {
		t.Fatalf("%s failed: %s", message.ToolName, message.ToolError)
	}
	var reply shellResponse
	if err := json.Unmarshal([]byte(message.Text), &reply); err != nil {
		t.Fatalf("invalid shell response %q: %v", message.Text, err)
	}
	return reply
}

func psQuote(path string) string {
	return strings.ReplaceAll(path, "'", "''")
}

func assertContents(t *testing.T, path, wanted string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != wanted {
		t.Fatalf("%s contains %q, error = %v, want %q", path, data, err, wanted)
	}
}
