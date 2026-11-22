package tool

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWorkspaceGrantRestoresOriginalSecurityAndDoesNotPropagate(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child.txt")
	if err := os.WriteFile(child, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	open := func(path string) windows.Handle {
		t.Helper()
		h, _, err := openWorkspaceObject(path, windows.MAXIMUM_ALLOWED, workspaceReadShare)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { windows.CloseHandle(h) })
		return h
	}
	security := func(handle windows.Handle) *windows.SECURITY_DESCRIPTOR {
		t.Helper()
		descriptor, err := workspaceSecurityDescriptor(handle)
		if err != nil {
			t.Fatal(err)
		}
		return descriptor
	}
	rootHandle := open(root)
	childHandle := open(child)
	rootOriginal := security(rootHandle)
	childOriginal := security(childHandle).String()
	sid, err := windows.StringToSid("S-1-5-21-2947453965-2637215858-375037998-3313187548")
	if err != nil {
		t.Fatal(err)
	}
	if err = grantWorkspaceSecurity(rootHandle, rootOriginal, sid, true, true); err != nil {
		t.Fatal(err)
	}
	if state := security(rootHandle).String(); !strings.Contains(state, sid.String()) || !strings.Contains(state, ";;;LW)") {
		t.Errorf("workspace has incomplete permissions: %s", state)
	}
	if state := security(childHandle).String(); state != childOriginal {
		t.Error("workspace grant propagated to an existing child")
	}
	created := filepath.Join(root, "created.txt")
	if err = os.WriteFile(created, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	createdHandle := open(created)
	if state := security(createdHandle).String(); !strings.Contains(state, sid.String()) {
		t.Error("new child did not inherit workspace capability")
	}
	rootDACL, _, err := rootOriginal.DACL()
	if err != nil {
		t.Fatal(err)
	}
	rootLabel, err := workspaceLabel(rootOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if err = applyWorkspaceSecurity(rootHandle, rootDACL, rootLabel); err != nil {
		t.Fatal(err)
	}
	createdDACL, _, err := security(createdHandle).DACL()
	if err != nil {
		t.Fatal(err)
	}
	_, err = stripWorkspaceCapability(createdDACL, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err = applyWorkspaceSecurity(createdHandle, createdDACL, nil); err != nil {
		t.Fatal(err)
	}
	if state := security(createdHandle).String(); strings.Contains(state, sid.String()) {
		t.Error("capability ACE could not be removed from a new child")
	}
	if restored := security(rootHandle).String(); strings.Split(restored, "S:")[0] != rootOriginal.String() || strings.Contains(restored, ";;;LW)") {
		t.Error("workspace original permissions differ after restoration")
	}
}

func TestWorkspacePreparationAndCleanup(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	root := t.TempDir()
	child := filepath.Join(root, "existing.txt")
	outbound := filepath.Join(root, "relocated.txt")
	deleted := filepath.Join(root, "deleted.txt")
	for _, path := range []string{child, outbound, deleted} {
		if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := workspaceTestSecurity(t, child)
	outboundBefore := workspaceTestSecurity(t, outbound)
	grant, err := prepareWorkspace(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := grant.cleanup(); err != nil {
			t.Error(err)
		}
	})
	if grant.workspaceSID.Equals(grant.tempSID) {
		t.Fatal("workspace and temporary capability SIDs are the same")
	}
	assertWorkspaceTestGrant(t, child, grant.workspaceSID)
	assertWorkspaceTestGrant(t, grant.tempPath, grant.tempSID)
	if _, err := prepareWorkspace(context.Background(), root); err == nil || !strings.Contains(err.Error(), "overlaps an active") {
		t.Fatalf("concurrent workspace preparation: %v", err)
	}
	moved := filepath.Join(root, "moved.txt")
	if err := os.Rename(child, moved); err != nil {
		t.Fatal(err)
	}
	// Trusted host code can relocate an object outside the workspace while
	// a grant is active. Recovery must still find its unchanged NTFS ID.
	movedOutside := filepath.Join(t.TempDir(), "relocated.txt")
	if err := os.Rename(outbound, movedOutside); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	newChild := filepath.Join(root, "new.txt")
	if err := os.WriteFile(newChild, []byte("new data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grant.tempPath, "tmp.txt"), []byte("temp data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := grant.cleanup(); err != nil {
		t.Fatal(err)
	}
	if restored := workspaceTestSecurity(t, moved); strings.Split(restored, "S:")[0] != before || strings.Contains(restored, ";;;LW)") {
		t.Error("renamed file did not recover previous permissions")
	}
	if restored := workspaceTestSecurity(t, movedOutside); strings.Split(restored, "S:")[0] != strings.Split(outboundBefore, "S:")[0] || strings.Contains(restored, ";;;LW)") {
		t.Error("relocated file did not recover previous permissions")
	}
	if newDescriptor := workspaceTestSecurity(t, newChild); strings.Contains(newDescriptor, grant.workspaceSID.String()) || strings.Contains(newDescriptor, ";;;LW)") {
		t.Error("new workspace file retained sandbox permissions")
	}
	if _, err := os.Stat(grant.tempPath); !os.IsNotExist(err) {
		t.Errorf("private temp was retained: %v", err)
	}
}

func TestWorkspaceRecoversAnInterruptedGrant(t *testing.T) {
	if root := os.Getenv("FOCUS_FLASH_GRANT_RECOVERY_FIXTURE"); root != "" {
		grant, err := prepareWorkspace(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "old_grant"), []byte(grant.workspaceSID.String()), 0600); err != nil {
			t.Fatal(err)
		}
		// Deliberately omit cleanup to model a host exiting unexpectedly.
		return
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	root := t.TempDir()
	ctx := context.Background()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkspaceRecoversAnInterruptedGrant$")
	command.Env = append(os.Environ(), "FOCUS_FLASH_GRANT_RECOVERY_FIXTURE="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("interrupted grant fixture: %v\n%s", err, output)
	}
	oldGrant, err := os.ReadFile(filepath.Join(root, "old_grant"))
	if err != nil {
		t.Fatal(err)
	}
	grant, err := prepareWorkspace(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := grant.cleanup(); err != nil {
			t.Error(err)
		}
	}()
	if state := workspaceTestSecurity(t, root); strings.Contains(state, string(oldGrant)) {
		t.Error("interrupted workspace still holds its old capability")
	}
}

func TestWorkspaceRejectsHardLinkedFileBeforeApplyingGrant(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	root := t.TempDir()
	other := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(other, []byte("outside data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(other, filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	before := workspaceTestSecurity(t, other)
	if _, err := prepareWorkspace(context.Background(), root); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("hard link authorization: %v", err)
	}
	if workspaceTestSecurity(t, other) != before {
		t.Error("outside hard link security changed")
	}
}

func TestWorkspaceRejectsDirectoryJunctionBeforeApplyingGrant(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	root := t.TempDir()
	target := t.TempDir()
	junction := filepath.Join(root, "junction")
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(system, "cmd.exe"), "/d", "/c", "mklink", "/j", junction, target)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create fixture junction: %v\n%s", err, output)
	}
	before := workspaceTestSecurity(t, target)
	if _, err := prepareWorkspace(context.Background(), root); err == nil || !strings.Contains(err.Error(), "redirected") {
		t.Fatalf("directory junction authorization: %v", err)
	}
	if workspaceTestSecurity(t, target) != before {
		t.Error("outside junction target security changed")
	}
}

func TestWorkspaceRollsBackWhenAChildCannotBeRelabeled(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	root := t.TempDir()
	child := filepath.Join(root, "protected.txt")
	if err := os.WriteFile(child, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := openWorkspaceObject(child, windows.MAXIMUM_ALLOWED, workspaceFullShare)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := workspaceSecurityDescriptor(h)
	if err != nil {
		windows.CloseHandle(h)
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		windows.CloseHandle(h)
		t.Fatal(err)
	}
	denied, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.WRITE_OWNER,
		AccessMode:        windows.DENY_ACCESS,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid)},
	}}, dacl)
	runtime.KeepAlive(user)
	if err != nil {
		windows.CloseHandle(h)
		t.Fatal(err)
	}
	err = windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, denied, nil)
	windows.CloseHandle(h)
	if err != nil {
		t.Fatal(err)
	}
	rootBefore := workspaceTestSecurity(t, root)
	childBefore := workspaceTestSecurity(t, child)
	grant, err := prepareWorkspace(context.Background(), root)
	if err == nil {
		_ = grant.cleanup()
		t.Fatal("permission preparation unexpectedly succeeded")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("unexpected setup error: %v", err)
	}
	if current := workspaceTestSecurity(t, root); strings.Split(current, "S:")[0] != rootBefore || strings.Contains(current, ";;;LW)") {
		t.Error("workspace root still has temporary security settings after failure")
	}
	if current := workspaceTestSecurity(t, child); current != childBefore {
		t.Error("protected child security changed before denial")
	}
}

func TestWindowsWorkspaceGrantsLimitTokenWrites(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	ctx := context.Background()
	root := t.TempDir()
	otherRoot := t.TempDir()
	existing := filepath.Join(root, "existing.txt")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	otherExisting := filepath.Join(otherRoot, "other.txt")
	for _, path := range []string{existing, outside, otherExisting} {
		if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	grant, err := prepareWorkspace(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := grant.cleanup(); err != nil {
			t.Error(err)
		}
	})
	other, err := prepareWorkspace(ctx, otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := other.cleanup(); err != nil {
			t.Error(err)
		}
	})
	primary, err := windowsExecutionToken("workspace-write", grant.workspaceSID, grant.tempSID, grant.tempPath)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	var impersonated windows.Token
	if err = windows.DuplicateTokenEx(primary, windows.TOKEN_IMPERSONATE|windows.TOKEN_QUERY, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &impersonated); err != nil {
		t.Fatal(err)
	}
	defer impersonated.Close()

	tests := []struct {
		path        string
		access      uint32
		disposition uint32
		allowed     bool
	}{
		{existing, windows.GENERIC_WRITE, windows.OPEN_EXISTING, true},
		{filepath.Join(root, "new.txt"), windows.GENERIC_WRITE, windows.CREATE_NEW, true},
		{filepath.Join(grant.tempPath, "new.txt"), windows.GENERIC_WRITE, windows.CREATE_NEW, true},
		{outside, windows.GENERIC_WRITE, windows.OPEN_EXISTING, false},
		{otherExisting, windows.GENERIC_WRITE, windows.OPEN_EXISTING, false},
		{filepath.Join(other.tempPath, "new.txt"), windows.GENERIC_WRITE, windows.CREATE_NEW, false},
		{otherExisting, windows.WRITE_DAC, windows.OPEN_EXISTING, false},
		{otherExisting, windows.WRITE_OWNER, windows.OPEN_EXISTING, false},
	}
	for _, test := range tests {
		err := func() error {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			if err := windows.SetThreadToken(nil, impersonated); err != nil {
				return err
			}
			defer func() {
				if err := windows.RevertToSelf(); err != nil {
					panic(err)
				}
			}()
			name := windows.StringToUTF16Ptr(test.path)
			file, err := windows.CreateFile(name, test.access, workspaceFullShare, nil,
				test.disposition, windows.FILE_ATTRIBUTE_NORMAL, 0)
			if err != nil {
				return err
			}
			return windows.CloseHandle(file)
		}()
		if (err == nil) != test.allowed {
			t.Errorf("access %x to %s, want %t: %v", test.access, test.path, test.allowed, err)
		}
	}
}

func workspaceTestSecurity(t *testing.T, path string) string {
	t.Helper()
	handle, _, err := openWorkspaceObject(path, windows.READ_CONTROL, workspaceFullShare)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	descriptor, err := workspaceSecurityDescriptor(handle)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor.String()
}

func assertWorkspaceTestGrant(t *testing.T, path string, sid *windows.SID) {
	t.Helper()
	state := workspaceTestSecurity(t, path)
	if !strings.Contains(state, sid.String()) || !strings.Contains(state, ";;;LW)") {
		t.Errorf("%s is missing the expected capability and integrity label", path)
	}
}
