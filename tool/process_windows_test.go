package tool

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func TestPowerShellVersionSelection(t *testing.T) {
	powershell, err := trustedPowerShell(t.TempDir())
	if err != nil || filepath.Base(powershell) != "pwsh.exe" {
		t.Fatalf("PowerShell 7 selection = %q, %v", powershell, err)
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	if hasPowerShell7Version(legacy) {
		t.Fatal("Windows PowerShell satisfied the PowerShell 7 version check")
	}
}

func TestWindowsProcessPipesAndExitCode(t *testing.T) {
	process := openTestPowerShell(t, "danger-full-access", t.TempDir(), `
$line = [Console]::ReadLine()
[Console]::WriteLine("output: $line")
[Console]::Error.WriteLine('error output')
exit 23
`)
	if _, err := io.WriteString(process.Stdin, "test input\n"); err != nil {
		t.Fatal(err)
	}
	if err := process.Stdin.Close(); err != nil {
		t.Fatal(err)
	}
	code, output, err := waitTestProcess(t, process)
	if err != nil || code != 23 || !strings.Contains(output, "output: test input") || !strings.Contains(output, "error output") {
		t.Fatalf("PowerShell exit %d, wait error %v, output %q", code, err, output)
	}
}

func TestWindowsReadOnlyProcessReadsButCannotWrite(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "original.txt")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	// Use cmd.exe directly to check native permissions independently of .NET
	// initialization. The normal tool path always starts PowerShell 7.
	process := openTestNativeCommand(t, "read-only", root, `type original.txt & echo changed>original.txt & echo new>unexpected.txt & del /q original.txt & ren original.txt renamed.txt`, workspaceGrant{})
	_, output, err := waitTestProcess(t, process)
	if err != nil || !strings.HasPrefix(output, "original") {
		t.Fatalf("native read-only process wait error %v, output %q", err, output)
	}
	content, err := os.ReadFile(file)
	if err != nil || string(content) != "original" {
		t.Fatalf("read original file = %q, %v", content, err)
	}
	for _, name := range []string{"unexpected.txt", "renamed.txt"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("file %s should be absent: %v", name, err)
		}
	}
}

func TestWindowsWorkspaceWriteProcessLimitsWrites(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	workspace := t.TempDir()
	otherWorkspace := t.TempDir()
	grant := prepareTestWindowsProcessWorkspace(t, workspace)
	otherGrant := prepareTestWindowsProcessWorkspace(t, otherWorkspace)
	otherWorkspaceFile := filepath.Join(otherWorkspace, "unexpected.txt")
	otherTempFile := filepath.Join(otherGrant.tempPath, "unexpected.txt")
	outsideFile := filepath.Join(t.TempDir(), "unexpected.txt")
	script := `echo workspace>allowed.txt & echo temporary>"%TEMP%\allowed.txt"`
	for _, target := range []string{otherWorkspaceFile, otherTempFile, outsideFile} {
		script += ` & echo unexpected>"` + target + `"`
	}
	process := openTestNativeCommand(t, "workspace-write", workspace, script, grant)
	if _, _, err := waitTestProcess(t, process); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(workspace, "allowed.txt"):      "workspace",
		filepath.Join(grant.tempPath, "allowed.txt"): "temporary",
	} {
		data, err := os.ReadFile(path)
		if err != nil || strings.TrimSpace(string(data)) != want {
			t.Fatalf("read %s: %q, %v", path, data, err)
		}
	}
	for _, target := range []string{otherWorkspaceFile, otherTempFile, outsideFile} {
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("file %s should be absent: %v", target, err)
		}
	}
}

func TestWindowsReadOnlyPowerShell(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "original.txt")
	if err := os.WriteFile(file, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(root, "unexpected.txt")
	// Relative paths deliberately test the actual runtime boundary independently
	// from shell admission. The only targets are disposable fixture files.
	process := openTestPowerShell(t, "read-only", root, `
$ErrorActionPreference = 'Stop'
$file = Join-Path $PWD original.txt
if ([IO.File]::ReadAllText($file) -ne 'original') { exit 80 }
try { [IO.File]::WriteAllText($file, 'overwritten'); exit 81 }
catch [UnauthorizedAccessException] { [Console]::WriteLine('write denied') }
try { [IO.File]::WriteAllText((Join-Path $PWD unexpected.txt), 'created'); exit 82 }
catch [UnauthorizedAccessException] { [Console]::WriteLine('create denied') }
try { [IO.File]::Delete($file); exit 83 }
catch [UnauthorizedAccessException] { [Console]::WriteLine('delete denied') }
exit 0
`)
	code, output, err := waitTestProcess(t, process)
	if knownRestrictedPowerShell7Failure(code, output) {
		t.Skip("PowerShell 7 BCrypt initialization fails under this Windows build's restricted token")
	}
	if err != nil || code != 0 || !strings.Contains(output, "write denied") || !strings.Contains(output, "create denied") || !strings.Contains(output, "delete denied") {
		t.Fatalf("PowerShell exit %d, wait error %v, output %q", code, err, output)
	}
	content, err := os.ReadFile(file)
	if err != nil || string(content) != "original" {
		t.Fatalf("read original file = %q, %v", content, err)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly created: %v", err)
	}
}

func knownRestrictedPowerShell7Failure(code int, output string) bool {
	if code != -532462766 || !strings.Contains(output, "BCrypt.dll") || !strings.Contains(output, "0x8007045A") || windows.RtlGetVersion().BuildNumber != 26200 {
		return false
	}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	revision, _, err := key.GetIntegerValue("UBR")
	return err == nil && revision == 9974
}

func TestWindowsProcessReapsDescendants(t *testing.T) {
	exe, err := trustedPowerShell("")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$info = New-Object Diagnostics.ProcessStartInfo
$info.FileName = '%s'
$info.Arguments = '-NoProfile -Command Start-Sleep -Seconds 60'
$info.UseShellExecute = $false
$info.CreateNoWindow = $true
$child = [Diagnostics.Process]::Start($info)
[Console]::WriteLine($child.Id)
[void][Console]::ReadLine()
exit 0
`, strings.ReplaceAll(exe, "'", "''"))
	process := openTestPowerShell(t, "danger-full-access", t.TempDir(), script)
	line, err := bufio.NewReader(process.Output).ReadString('\n')
	if err != nil {
		t.Fatalf("read child pid %q: %v", line, err)
	}
	pid, err := strconv.ParseUint(strings.TrimSpace(line), 10, 32)
	if err != nil {
		t.Fatalf("parse child pid %q: %v", line, err)
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("open child process: %v", err)
	}
	defer windows.CloseHandle(child)
	if err := process.Stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if code, err := process.Wait(); code != 0 || err != nil {
		t.Fatalf("PowerShell exit %d, %v", code, err)
	}
	if event, err := windows.WaitForSingleObject(child, 2000); event != windows.WAIT_OBJECT_0 || err != nil {
		t.Fatalf("child is still active after waiting: status %d, %v", event, err)
	}
}

func openTestPowerShell(t *testing.T, mode, root, script string) *windowsProcess {
	t.Helper()
	powershell, err := trustedPowerShell(root)
	if err != nil {
		t.Fatal(err)
	}
	process, err := startWindowsProcess(powershell, mode, root, script, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	trackTestProcess(t, process)
	return process
}

func openTestNativeCommand(t *testing.T, mode, root, script string, grant workspaceGrant) *windowsProcess {
	t.Helper()
	systemDir, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(systemDir, "cmd.exe")
	command, err := windows.UTF16PtrFromString(windows.EscapeArg(exe) + ` /d /c "` + script + `"`)
	if err != nil {
		t.Fatal(err)
	}
	process, err := startWindowsExecutable(mode, root, exe, command, grant.workspaceSID, grant.tempSID, grant.tempPath)
	if err != nil {
		t.Fatal(err)
	}
	trackTestProcess(t, process)
	return process
}

func prepareTestWindowsProcessWorkspace(t *testing.T, root string) workspaceGrant {
	t.Helper()
	grant, err := prepareWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := grant.cleanup(); err != nil {
			t.Error(err)
		}
	})
	return grant
}

func trackTestProcess(t *testing.T, process *windowsProcess) {
	t.Helper()
	t.Cleanup(func() {
		process.Kill()
		process.Wait()
		process.Stdin.Close()
		process.Output.Close()
	})
}

func waitTestProcess(t *testing.T, process *windowsProcess) (int, string, error) {
	t.Helper()
	type stream struct {
		content []byte
		err     error
	}
	drained := make(chan stream, 1)
	go func() {
		output, err := io.ReadAll(process.Output)
		drained <- stream{content: output, err: err}
	}()
	code, err := process.Wait()
	if !process.treeStopped {
		t.Fatalf("Windows job was not verified closed: %v", err)
	}
	output := <-drained
	if output.err != nil {
		t.Fatalf("read PowerShell output: %v", output.err)
	}
	return code, string(output.content), err
}
