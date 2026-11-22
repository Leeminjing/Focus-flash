package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestEditReplacesUniqueUTF8Text(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	const scriptText = "$([Console]::WriteLine('as data'))"
	tests := []struct {
		name     string
		original string
		oldText  string
		newText  string
		want     string
		failure  string
	}{
		{
			name: "UTF8 BOM and CRLF", original: bom + "保留🙂\r\n旧\r\n文本\r\n结尾",
			oldText: "旧\n文本", newText: scriptText + "\n新", want: bom + "保留🙂\r\n" + scriptText + "\r\n新\r\n结尾",
		},
		{
			name: "date-shaped text", original: "2026-11-21T12:12:12Z\n",
			oldText: "2026-11-21T12:12:12Z", newText: "2027-01-01T00:00:00Z", want: "2027-01-01T00:00:00Z\n",
		},
		{
			name: "mixed line endings", original: "one\r\ntwo\nthree",
			oldText: "two\n", newText: "four\n", want: "one\r\nfour\nthree",
		},
		{
			name: "delete whole match", original: "delete me", oldText: "delete me", want: "",
		},
		{
			name: "duplicate overlapping matches", original: "aaaa", oldText: "aa", newText: "b",
			want: "aaaa", failure: "occurs more than once",
		},
		{
			name: "case-sensitive match", original: "Hello", oldText: "hello", newText: "hi",
			want: "Hello", failure: "was not found",
		},
		{
			name: "reject invalid UTF8", original: "hello\xff", oldText: "hello", newText: "hi",
			want: "hello\xff", failure: "valid UTF-8",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "it's a [literal] filename.txt")
			if err := os.WriteFile(path, []byte(test.original), 0600); err != nil {
				t.Fatal(err)
			}
			output, err := runWindowsEdit(context.Background(), editArgs{Path: path, OldText: test.oldText, NewText: test.newText}, sandboxFullAccess, workspaceGrant{})
			if test.failure == "" {
				if err != nil {
					t.Fatalf("edit failed: %v\n%s", err, output)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.failure) {
				t.Fatalf("want failure containing %q, got %v\n%s", test.failure, err, output)
			}
			checkEditDirectory(t, path, test.want)
		})
	}
}

func TestEditRefusesFileOpenForWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "busy.txt")
	writer, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.WriteString("original"); err != nil {
		t.Fatal(err)
	}
	if output, err := runWindowsEdit(context.Background(), editArgs{Path: path, OldText: "original", NewText: "replacement"}, sandboxFullAccess, workspaceGrant{}); err == nil {
		t.Fatalf("edit succeeded despite existing writer: %s", output)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	checkEditDirectory(t, path, "original")
}

func TestEditFailedReplacementKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "read-only.txt")
	if err := os.WriteFile(path, []byte("original"), 0400); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0600)
	if output, err := runWindowsEdit(context.Background(), editArgs{Path: path, OldText: "original", NewText: "replacement"}, sandboxFullAccess, workspaceGrant{}); err == nil {
		t.Fatalf("edit succeeded despite read-only file attribute: %s", output)
	}
	checkEditDirectory(t, path, "original")
}

func TestEditWorkspaceWriteUsesWindowsPermissions(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	workspace := t.TempDir()
	inside := filepath.Join(workspace, "inside.txt")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	original := editDACLForTest(t, inside)
	ctx := context.Background()
	grant, err := prepareWorkspace(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := grant.cleanup(); err != nil {
			t.Error(err)
		}
		if !t.Failed() && editDACLForTest(t, inside) != original {
			t.Error("workspace cleanup did not restore the original file DACL")
		}
	})
	before := editDACLForTest(t, inside)
	if _, err := runWindowsEdit(ctx, editArgs{Path: inside, OldText: "original", NewText: "replacement"}, sandboxWorkspaceWrite, grant); err != nil {
		t.Fatal(err)
	}
	if _, err := runWindowsEdit(ctx, editArgs{Path: outside, OldText: "original", NewText: "replacement"}, sandboxWorkspaceWrite, grant); err == nil {
		t.Fatal("workspace-write edit changed an outside file")
	}
	checkEditDirectory(t, inside, "replacement")
	checkEditDirectory(t, outside, "original")
	if after := editDACLForTest(t, inside); before != after {
		t.Fatal("the edit changed the file DACL")
	}
}

func editDACLForTest(t *testing.T, path string) string {
	t.Helper()
	h, _, err := openWorkspaceObject(path, windows.MAXIMUM_ALLOWED, workspaceFullShare)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

func checkEditDirectory(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("file content = %q, want %q", got, want)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("unexpected files after edit: %v", entries)
	}
}
