package tool

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWindowsPowerShellConsoleTextRoundTrip(t *testing.T) {
	page, _, _ := consoleCodePage.Call()
	if page != 936 && page != 65001 {
		t.Skipf("the Chinese text fixture requires code page 936 or 65001, got %d", page)
	}
	workspace := t.TempDir()
	powershell, err := trustedPowerShell(workspace)
	if err != nil {
		t.Fatal(err)
	}
	process, err := startWindowsProcess(powershell, string(sandboxFullAccess), workspace,
		"[Console]::WriteLine([Console]::ReadLine())", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	session := newShellSession(process)
	t.Cleanup(func() {
		if err := session.stop(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := session.send(ctx, "你好\n", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	output, truncated := session.output.take()
	if strings.TrimSpace(output) != "你好" || truncated || session.waitErr != nil || session.exitCode != 0 {
		t.Fatalf("exit %d, error %v, output %q, truncated %t", session.exitCode, session.waitErr, output, truncated)
	}
}
