package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestShellAdmission(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "work area")
	sibling := filepath.Join(parent, "work area outside")
	for _, name := range []string{workspace, sibling, filepath.Join(workspace, "subdir")} {
		if err := os.Mkdir(name, 0700); err != nil {
			t.Fatal(err)
		}
	}
	powershell, err := trustedPowerShell(workspace)
	if err != nil {
		t.Fatal(err)
	}
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	systemCommand := filepath.Join(systemDirectory, "hostname.exe")
	for _, tt := range []struct {
		name    string
		command string
		blocked bool
	}{
		{name: "normal Go command", command: "go test ./..."},
		{name: "quoted workspace file", command: "Get-Content '" + filepath.Join(workspace, "new file.txt") + "'"},
		{name: "Unicode workspace file", command: "Get-Content '" + filepath.Join(workspace, "工作区.txt") + "'"},
		{name: "workspace file pattern", command: `Get-ChildItem '.\subdir\*.go'`},
		{name: "continue in child directory", command: "cd -LiteralPath subdir; Get-ChildItem"},
		{name: "path environment assignment", command: "$env:PATH = '.;' + $env:PATH"},
		{name: "path environment provider", command: "Set-Item Env:PATH '.'"},
		{name: "ordinary subexpression", command: "Write-Output $(Get-ChildItem .)"},
		{name: "PowerShell 7 chain", command: "Write-Output first && Write-Output second"},
		{name: "range operator", command: "Write-Output (1..3)"},
		{name: "HTTPS URL", command: "Write-Output 'https://example.com/resource'"},
		{name: "trusted executable target", command: "& '" + systemCommand + "'"},
		{name: "outside absolute", command: "Get-Content '" + sibling + "\\fixture.txt'", blocked: true},
		{name: "outside redirection", command: "Write-Output input > '" + sibling + "\\fixture.txt'", blocked: true},
		{name: "system executable as a path", command: "Get-Content '" + systemCommand + "'", blocked: true},
		{name: "parent segment", command: `Get-Content 'dir\..\fixture.txt'`, blocked: true},
		{name: "file URI", command: "Write-Output 'FILE://host/folder'", blocked: true},
		{name: "UNC", command: `Get-Content '\\server\share\fixture.txt'`, blocked: true},
		{name: "non-file provider", command: `Get-Item 'HKCU:\Example'`, blocked: true},
		{name: "drive-relative", command: `Get-Item 'C:fixture.txt'`, blocked: true},
		{name: "drive-relative wildcard", command: `Get-Item 'C:*'`, blocked: true},
		{name: "current-drive absolute", command: `Get-Item '\fixture.txt'`, blocked: true},
		{name: "directory without argument", command: "cd", blocked: true},
		{name: "directory variable", command: "Set-Location $env:USERPROFILE", blocked: true},
		{name: "nested directory command", command: "Write-Output $(pushd subdir)", blocked: true},
		{name: "stack restore", command: "Pop-Location", blocked: true},
		{name: "directory history", command: "cd -", blocked: true},
		{name: "directory home", command: "chdir ~", blocked: true},
		{name: "module-qualified location", command: "Microsoft.PowerShell.Management\\Set-Location '" + sibling + "'", blocked: true},
		{name: "invalid syntax", command: "Write-Output (", blocked: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateShellCommand(context.Background(), powershell, tt.command, workspace, workspace, sandboxReadOnly)
			if (err != nil) != tt.blocked {
				t.Fatalf("blocked = %t, want %t, error = %v", err != nil, tt.blocked, err)
			}
		})
	}
}

func TestShellAdmissionWorkingDirectory(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := validateShellCommand(context.Background(), "", "Get-Location", workspace, outside, sandboxWorkspaceWrite); err == nil {
		t.Fatal("workspace-write admitted an outside working directory")
	}
	if err := validateShellCommand(context.Background(), "", "Get-Location", workspace, outside, sandboxFullAccess); err != nil {
		t.Fatalf("danger-full-access rejected an outside working directory: %v", err)
	}
}

func TestShellAdmissionChecksDirectoryJunction(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(workspace, "linked")
	powershell, err := trustedPowerShell(workspace)
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	create := "New-Item -ItemType Junction -Path " + quote(link) + " -Target " + quote(outside)
	command := exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", create)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create junction fixture: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	if _, err := validateWorkspacePath(workspace, filepath.Join(link, "new.txt")); err == nil {
		t.Fatal("admitted a path whose nearest existing ancestor points outside the workspace")
	}
	if err := validateShellCommand(context.Background(), powershell, "Get-ChildItem linked", workspace, workspace, sandboxReadOnly); err == nil {
		t.Fatal("admitted a literal that names a junction to an outside directory")
	}
}

func TestShellAdmissionUsesPowerShell7Parser(t *testing.T) {
	powershell, err := trustedPowerShell(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseShellAdmission(context.Background(), powershell, "$env:PATH = '.;' + $env:PATH")
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.ChangesPath {
		t.Fatal("parser did not recognize PATH assignment")
	}
	parsed, err = parseShellAdmission(context.Background(), powershell, "[Environment]::SetEnvironmentVariable('PATH', '.')")
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.ChangesPath {
		t.Fatal("parser did not recognize the environment setter for PATH")
	}

	marker := filepath.Join(t.TempDir(), "parser-must-not-create")
	command := "Set-Content -LiteralPath '" + strings.ReplaceAll(marker, "'", "''") + "' -Value 'unexpected'"
	if _, err := parseShellAdmission(context.Background(), powershell, command); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("PowerShell parser caused a file side effect: %v", err)
	}
}
