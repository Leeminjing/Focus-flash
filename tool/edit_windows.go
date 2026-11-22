package tool

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

type editArgs struct {
	Path    string
	OldText string
	NewText string
}

var replaceWindowsFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func runWindowsEdit(ctx context.Context, args editArgs, mode sandboxMode, grant workspaceGrant) (string, error) {
	token, err := windowsExecutionToken(string(mode), grant.workspaceSID, grant.tempSID, grant.tempPath)
	if err != nil {
		return "", err
	}
	if token == 0 {
		return editWindowsFile(ctx, args, nil)
	}
	defer token.Close()

	var threadToken windows.Token
	if err := windows.DuplicateTokenEx(token, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE, nil,
		windows.SecurityImpersonation, windows.TokenImpersonation, &threadToken); err != nil {
		return "", fmt.Errorf("duplicate Windows edit token: %w", err)
	}
	defer threadToken.Close()

	// Open and replace files on one OS thread so every access check sees the
	// same restricted token. Wait for an in-progress commit to finish recovery.
	type editResult struct {
		message string
		err     error
	}
	done := make(chan editResult, 1)
	go func() {
		runtime.LockOSThread()
		if err := windows.SetThreadToken(nil, threadToken); err != nil {
			runtime.UnlockOSThread()
			done <- editResult{err: fmt.Errorf("apply Windows edit token: %w", err)}
			return
		}
		var result editResult
		defer func() {
			if err := windows.RevertToSelf(); err != nil {
				// Returning from the goroutine while locked retires its OS thread.
				result.err = errors.Join(result.err, fmt.Errorf("release Windows edit token: %w", err))
			} else {
				runtime.UnlockOSThread()
			}
			done <- result
		}()
		result.message, result.err = editWindowsFile(ctx, args, grant.workspaceSID)
	}()
	result := <-done
	return result.message, result.err
}

func editWindowsFile(ctx context.Context, args editArgs, workspaceSID *windows.SID) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if args.OldText == "" || !utf8.ValidString(args.OldText) || !utf8.ValidString(args.NewText) {
		return "", errors.New("old_text must be nonempty and edit text must be UTF-8")
	}
	path, err := editWindowsPath(args.Path)
	if err != nil {
		return "", err
	}
	original, err := openEditSource(path, args.Path)
	if err != nil {
		return "", err
	}
	defer original.Close()
	snapshot, err := io.ReadAll(original)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(snapshot) {
		return "", errors.New("the file must contain valid UTF-8 text")
	}
	const bom = "\xef\xbb\xbf"
	text := string(snapshot)
	offset := 0
	if strings.HasPrefix(text, bom) {
		offset = len(bom)
		text = text[offset:]
	}
	oldText, newText := args.OldText, args.NewText
	if ending := editLineEnding(text); ending != "" {
		oldText = strings.ReplaceAll(strings.ReplaceAll(oldText, "\r\n", "\n"), "\n", ending)
		newText = strings.ReplaceAll(strings.ReplaceAll(newText, "\r\n", "\n"), "\n", ending)
	}
	match := strings.Index(text, oldText)
	if match == -1 {
		return "", errors.New("old_text was not found")
	}
	if strings.Contains(text[match+1:], oldText) {
		return "", errors.New("old_text occurs more than once")
	}
	if oldText == newText {
		return "The matched text already has the requested content.", nil
	}
	start := offset + match
	updated := make([]byte, 0, len(snapshot)-len(oldText)+len(newText))
	updated = append(updated, snapshot[:start]...)
	updated = append(updated, newText...)
	updated = append(updated, snapshot[start+len(oldText):]...)
	attributes, err := editStagingAttributes(windows.Handle(original.Fd()), workspaceSID)
	if err != nil {
		return "", fmt.Errorf("prepare staging file permissions: %w", err)
	}
	if err := commitWindowsEdit(ctx, args.Path, path, snapshot, updated, attributes); err != nil {
		return "", err
	}
	return "Replaced one occurrence.", nil
}

func editLineEnding(text string) string {
	withoutCRLF := strings.ReplaceAll(text, "\r\n", "")
	switch {
	case strings.Contains(withoutCRLF, "\r"), !strings.Contains(text, "\n"):
		return ""
	case !strings.Contains(text, "\r\n"):
		return "\n"
	case !strings.Contains(withoutCRLF, "\n"):
		return "\r\n"
	default:
		return ""
	}
}

func editWindowsPath(path string) (*uint16, error) {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	if !filepath.IsAbs(path) || strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) ||
		strings.Contains(strings.TrimPrefix(path, volume), ":") {
		return nil, errors.New("edit requires an absolute regular file path")
	}
	if len(path) >= 248 {
		if strings.HasPrefix(path, `\\`) {
			path = `\\?\UNC\` + strings.TrimPrefix(path, `\\`)
		} else {
			path = `\\?\` + path
		}
	}
	return windows.UTF16PtrFromString(path)
}

func openEditSource(path *uint16, name string) (*os.File, error) {
	// Exclude in-place writers while allowing readers and the final replacement.
	file, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("open edit target: %w", err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(file, &info); err != nil {
		windows.CloseHandle(file)
		return nil, fmt.Errorf("inspect edit target: %w", err)
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_DEVICE|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		windows.CloseHandle(file)
		return nil, errors.New("the edit target must be a regular file")
	}
	return os.NewFile(uintptr(file), name), nil
}

func editStagingAttributes(source windows.Handle, sid *windows.SID) (*windows.SecurityAttributes, error) {
	if sid == nil {
		return nil, nil
	}
	// ReplaceFile needs WRITE_DAC on its new file to transfer the target's
	// DACL. Grant it only on the disposable staging file.
	original, err := windows.GetSecurityInfo(source, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	acl, _, err := original.DACL()
	if err != nil {
		return nil, err
	}
	stagingACL, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.WRITE_DAC,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_UNKNOWN, TrusteeValue: windows.TrusteeValueFromSID(sid)},
	}}, acl)
	if err != nil {
		return nil, err
	}
	staging, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, err
	}
	if err := staging.SetDACL(stagingACL, true, false); err != nil {
		return nil, err
	}
	staging, err = staging.ToSelfRelative()
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: staging}, nil
}

func commitWindowsEdit(ctx context.Context, name string, path *uint16, snapshot, updated []byte, attributes *windows.SecurityAttributes) (err error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	base := filepath.Join(filepath.Dir(name), fmt.Sprintf(".focus-edit-%x", suffix))
	tempName, backupName := base+".tmp", base+".bak"
	temporary, err := editWindowsPath(tempName)
	if err != nil {
		return err
	}
	backup, err := editWindowsPath(backupName)
	if err != nil {
		return err
	}
	file, err := windows.CreateFile(temporary, windows.GENERIC_WRITE, 0, attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fmt.Errorf("stage edit: %w", err)
	}
	defer func() {
		if removeErr := windows.DeleteFile(temporary); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove temporary edit file %q: %w", tempName, removeErr))
		}
	}()
	staged := os.NewFile(uintptr(file), tempName)
	_, err = staged.Write(updated)
	if err == nil {
		err = staged.Sync()
	}
	if err = errors.Join(err, staged.Close()); err != nil {
		return fmt.Errorf("stage edit: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, _, replaceErr := replaceWindowsFile.Call(uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(temporary)),
		uintptr(unsafe.Pointer(backup)), 0, 0, 0)
	runtime.KeepAlive(path)
	if result == 0 {
		err := fmt.Errorf("replace edited file: %w", replaceErr)
		// If ReplaceFile moved the old file, restore it without replacing any
		// version installed by another writer since the failure.
		if _, attrErr := windows.GetFileAttributes(backup); attrErr == nil {
			if restoreErr := windows.MoveFile(backup, path); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("original retained in %q: %w", backupName, restoreErr))
			}
		} else if !errors.Is(attrErr, os.ErrNotExist) {
			return errors.Join(err, fmt.Errorf("inspect recovery copy %q: %w", backupName, attrErr))
		}
		return err
	}

	// A concurrent rename can change what was at the destination even with
	// in-place writers excluded. Preserve the actual displaced file on conflict.
	displaced, err := openEditSource(backup, backupName)
	if err != nil {
		return fmt.Errorf("edit applied but could not verify recovery copy %q: %w", backupName, err)
	}
	contents, err := io.ReadAll(displaced)
	if err = errors.Join(err, displaced.Close()); err != nil {
		return fmt.Errorf("edit applied but could not verify recovery copy %q: %w", backupName, err)
	}
	if !bytes.Equal(contents, snapshot) {
		return fmt.Errorf("destination changed during the edit. The edit was applied and the displaced contents are in %q", backupName)
	}
	if err := windows.DeleteFile(backup); err != nil {
		return fmt.Errorf("edit applied but could not remove recovery copy %q: %w", backupName, err)
	}
	return nil
}
