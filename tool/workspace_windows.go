package tool

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

type workspaceGrant struct {
	workspaceSID, tempSID *windows.SID
	tempPath              string
	cleanup               func() error
}

type workspaceSecurityBackup struct {
	Path       string
	ID         workspaceFileID
	Descriptor string
	Directory  bool
}

type workspaceGrantJournal struct {
	Root    string
	ID      workspaceFileID
	SID     string
	TempSID string
	Files   []workspaceSecurityBackup
}

// prepareWorkspace stages a write grant before any command starts. Its cleanup
// must run after all processes using the grant have stopped.
func prepareWorkspace(ctx context.Context, workspace string) (workspaceGrant, error) {
	var grant workspaceGrant
	root, rootID, err := workspaceGrantRoot(workspace)
	if err != nil {
		return grant, err
	}
	state, err := workspaceGrantState()
	if err != nil {
		return grant, err
	}
	if workspacePathWithin(root, state) || workspacePathWithin(state, root) {
		return grant, errors.New("workspace overlaps the Windows sandbox state directory")
	}
	manager, err := lockWorkspaceManager(ctx, state)
	if err != nil {
		return grant, err
	}
	defer windows.CloseHandle(manager)
	if err := recoverWorkspaceGrants(state, root, rootID); err != nil {
		return grant, err
	}
	grant.workspaceSID, err = newWindowsCapability()
	if err != nil {
		return workspaceGrant{}, err
	}
	grant.tempSID, err = newWindowsCapability()
	if err != nil {
		return workspaceGrant{}, err
	}
	id, err := windows.GenerateGUID()
	if err != nil {
		return workspaceGrant{}, err
	}
	base := filepath.Join(state, strings.Trim(id.String(), "{}"))
	grant.tempPath = base + ".temp"
	snapshot := workspaceGrantJournal{Root: root, ID: rootID, SID: grant.workspaceSID.String(), TempSID: grant.tempSID.String()}
	if err := visitWorkspace(ctx, root, func(path string, h windows.Handle, info windows.ByHandleFileInformation) error {
		if workspaceIdentity(info).Volume != rootID.Volume || info.NumberOfLinks != 1 {
			return errors.New("workspace contains a hard link or file on another volume")
		}
		descriptor, err := workspaceSecurityDescriptor(h)
		if err != nil {
			return err
		}
		dacl, _, err := descriptor.DACL()
		if err != nil || dacl == nil {
			return errors.New("workspace object has no usable DACL")
		}
		if err := validateWorkspaceLabel(descriptor); err != nil {
			return err
		}
		security := descriptor.String()
		if _, err := windows.SecurityDescriptorFromString(security); err != nil {
			return fmt.Errorf("record original security: %w", err)
		}
		snapshot.Files = append(snapshot.Files, workspaceSecurityBackup{
			Path: path, ID: workspaceIdentity(info), Descriptor: security,
			Directory: info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0,
		})
		return nil
	}); err != nil {
		return workspaceGrant{}, err
	}
	if len(snapshot.Files) == 0 || snapshot.Files[0].ID != rootID {
		return workspaceGrant{}, errors.New("workspace root changed during authorization")
	}
	lease, err := openWorkspaceLock(base+".lock", windows.CREATE_NEW)
	if err != nil {
		return workspaceGrant{}, err
	}
	if err := writeWorkspaceJournal(base, snapshot); err != nil {
		windows.CloseHandle(lease)
		return workspaceGrant{}, errors.Join(err, removeWorkspaceState(base+".lock"))
	}
	var once sync.Once
	var cleanupErr error
	grant.cleanup = func() error {
		once.Do(func() {
			manager, err := lockWorkspaceManager(context.Background(), state)
			if err != nil {
				cleanupErr = errors.Join(err, windows.CloseHandle(lease))
				return
			}
			defer windows.CloseHandle(manager)
			cleanupErr = finishWorkspaceGrant(base, snapshot, lease)
		})
		return cleanupErr
	}
	if err := grantWorkspaceObjects(ctx, snapshot, grant); err != nil {
		return workspaceGrant{}, errors.Join(err, finishWorkspaceGrant(base, snapshot, lease))
	}
	return grant, nil
}

func workspaceGrantRoot(workspace string) (string, workspaceFileID, error) {
	var id workspaceFileID
	path, err := filepath.Abs(workspace)
	if err != nil {
		return "", id, err
	}
	handle, info, err := openWorkspaceObject(path, windows.FILE_READ_ATTRIBUTES, workspaceFullShare)
	if err != nil {
		return "", id, err
	}
	defer windows.CloseHandle(handle)
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "", id, errors.New("workspace root must be a directory without a reparse point")
	}
	canonical, err := workspaceFinalPath(handle)
	if err != nil {
		return "", id, err
	}
	if strings.EqualFold(canonical, filepath.VolumeName(canonical)+`\`) {
		return "", id, errors.New("a drive root cannot be a writable workspace")
	}
	return canonical, workspaceIdentity(info), validateWorkspaceVolume(canonical, handle)
}

func workspaceGrantState() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(cache, "Focus-flash", "windows-sandbox")
	if err := os.MkdirAll(path, 0700); err != nil {
		return "", err
	}
	canonical, _, err := workspaceGrantRoot(path)
	return canonical, err
}

func newWindowsCapability() (*windows.SID, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	return windows.StringToSid(fmt.Sprintf("S-1-5-21-%d-%d-%d-%d", binary.LittleEndian.Uint32(random[:4]),
		binary.LittleEndian.Uint32(random[4:8]), binary.LittleEndian.Uint32(random[8:12]), binary.LittleEndian.Uint32(random[12:])))
}

func openWorkspaceLock(path string, disposition uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, disposition,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_ATTRIBUTE_NORMAL, 0)
}

func lockWorkspaceManager(ctx context.Context, state string) (windows.Handle, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		h, err := openWorkspaceLock(filepath.Join(state, "grants.lock"), windows.OPEN_ALWAYS)
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return h, err
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func writeWorkspaceJournal(base string, snapshot workspaceGrantJournal) error {
	pending := base + ".pending"
	file, err := os.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	encoded := json.NewEncoder(file).Encode(snapshot)
	if err := errors.Join(encoded, file.Sync(), file.Close()); err != nil {
		return errors.Join(err, removeWorkspaceState(pending))
	}
	from := windows.StringToUTF16Ptr(pending)
	to := windows.StringToUTF16Ptr(base + ".json")
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return errors.Join(err, removeWorkspaceState(pending))
	}
	return nil
}

func recoverWorkspaceGrants(state, root string, rootID workspaceFileID) error {
	entries, err := os.ReadDir(state)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		base := filepath.Join(state, strings.TrimSuffix(name, ".json"))
		data, err := os.ReadFile(base + ".json")
		if err != nil {
			return err
		}
		var snapshot workspaceGrantJournal
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return fmt.Errorf("read recovery journal %s: %w", name, err)
		}
		if rootID != snapshot.ID && !workspacePathWithin(root, snapshot.Root) && !workspacePathWithin(snapshot.Root, root) {
			continue
		}
		lease, err := openWorkspaceLock(base+".lock", windows.OPEN_ALWAYS)
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return fmt.Errorf("workspace overlaps an active Windows write grant for %s", snapshot.Root)
		}
		if err != nil {
			return err
		}
		if err := finishWorkspaceGrant(base, snapshot, lease); err != nil {
			return fmt.Errorf("recover write grant for %s: %w", snapshot.Root, err)
		}
	}
	return nil
}

func visitWorkspace(ctx context.Context, root string, visit func(string, windows.Handle, windows.ByHandleFileInformation) error) error {
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		handle, info, err := openWorkspaceObject(path, windows.MAXIMUM_ALLOWED, workspaceReadShare)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer windows.CloseHandle(handle)
		canonical, err := workspaceFinalPath(handle)
		if err != nil {
			return err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || !strings.EqualFold(canonical, path) {
			return fmt.Errorf("workspace object is redirected: %s", path)
		}
		if err := visit(path, handle, info); err != nil {
			return fmt.Errorf("authorize %s: %w", path, err)
		}
		return nil
	})
}

func grantWorkspaceObjects(ctx context.Context, snapshot workspaceGrantJournal, grant workspaceGrant) error {
	// Reopen each recorded pathname and recheck its file identity before taking
	// an action. A changed tree requires a retry with a new snapshot.
	for _, backup := range snapshot.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		handle, actual, err := openWorkspaceObject(backup.Path, windows.MAXIMUM_ALLOWED, workspaceReadShare)
		if err != nil {
			return err
		}
		err = func() error {
			defer windows.CloseHandle(handle)
			path, err := workspaceFinalPath(handle)
			if err != nil {
				return err
			}
			if !strings.EqualFold(path, backup.Path) || workspaceIdentity(actual) != backup.ID || actual.NumberOfLinks != 1 || actual.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return errors.New("workspace tree changed during authorization")
			}
			descriptor, err := workspaceSecurityDescriptor(handle)
			if err != nil {
				return err
			}
			if descriptor.String() != backup.Descriptor {
				return errors.New("workspace security changed during authorization")
			}
			return grantWorkspaceSecurity(handle, descriptor, grant.workspaceSID, backup.Directory, backup.ID == snapshot.ID)
		}()
		if err != nil {
			return fmt.Errorf("prepare %s: %w", backup.Path, err)
		}
	}
	if err := os.Mkdir(grant.tempPath, 0700); err != nil {
		return err
	}
	handle, _, err := openWorkspaceObject(grant.tempPath, windows.MAXIMUM_ALLOWED, workspaceReadShare)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	descriptor, err := workspaceSecurityDescriptor(handle)
	if err != nil {
		return err
	}
	return grantWorkspaceSecurity(handle, descriptor, grant.tempSID, true, true)
}
