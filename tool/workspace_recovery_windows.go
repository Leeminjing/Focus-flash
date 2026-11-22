package tool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func finishWorkspaceGrant(base string, snapshot workspaceGrantJournal, lease windows.Handle) error {
	restored := restoreWorkspaceGrant(snapshot)
	removed := removeWorkspaceTemp(base + ".temp")
	closed := windows.CloseHandle(lease)
	if err := errors.Join(restored, removed, closed); err != nil {
		return fmt.Errorf("Windows write grant recovery recorded in %s.json: %w", base, err)
	}
	if err := removeWorkspaceState(base + ".json"); err != nil {
		return err
	}
	return removeWorkspaceState(base + ".lock")
}

func restoreWorkspaceGrant(snapshot workspaceGrantJournal) error {
	workspaceSID, err := windows.StringToSid(snapshot.SID)
	if err != nil {
		return err
	}
	tempSID, err := windows.StringToSid(snapshot.TempSID)
	if err != nil {
		return err
	}
	volume, volumeInfo, err := openWorkspaceObject(filepath.VolumeName(snapshot.Root)+`\`, windows.FILE_READ_ATTRIBUTES, workspaceFullShare)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(volume)
	if volumeInfo.VolumeSerialNumber != snapshot.ID.Volume {
		return errors.New("workspace volume changed before permissions could be restored")
	}
	var restoreErr error
	var root string
	verifiedFileIDLookup := false
	priorSecurity := make(map[workspaceFileID]string, len(snapshot.Files))
	// Records are ordered parent first. NTFS may reinsert an inherited ACE if
	// the parent still grants it when the child is restored.
	for _, backup := range snapshot.Files {
		priorSecurity[backup.ID] = backup.Descriptor
		if backup.ID.Volume != volumeInfo.VolumeSerialNumber || backup.ID.Index == 0 {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("invalid recorded file ID for %s", backup.Path))
			continue
		}
		handle, pathInfo, pathErr := openWorkspaceObject(backup.Path, windows.MAXIMUM_ALLOWED, workspaceReadShare)
		if pathErr != nil || workspaceIdentity(pathInfo) != backup.ID {
			if pathErr == nil {
				windows.CloseHandle(handle)
			}
			handle, _, err = openWorkspaceObjectByID(volume, backup.ID)
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) && (pathErr == nil || errors.Is(pathErr, fs.ErrNotExist)) {
				// NTFS can return ERROR_INVALID_PARAMETER for a deleted file's
				// old ID. Require an absent or replaced path and verify that
				// the exact same lookup still works on the live volume root.
				if !verifiedFileIDLookup {
					witness, _, witnessErr := openWorkspaceObjectByID(volume, workspaceIdentity(volumeInfo))
					if witnessErr == nil {
						windows.CloseHandle(witness)
						verifiedFileIDLookup = true
					} else {
						err = errors.Join(err, fmt.Errorf("verify NTFS file ID lookup: %w", witnessErr))
					}
				}
				if verifiedFileIDLookup {
					continue
				}
			}
		} else {
			err = nil
		}
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			continue
		}
		if err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("reopen %s: %w", backup.Path, err))
			continue
		}
		err = removeWorkspaceCapability(handle, backup.Descriptor, workspaceSID, tempSID)
		if backup.ID == snapshot.ID && err == nil {
			root, err = workspaceFinalPath(handle)
		}
		windows.CloseHandle(handle)
		if err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore %s: %w", backup.Path, err))
		}
	}
	if root != "" {
		restoreErr = errors.Join(restoreErr, removeWorkspaceDescendantCapabilities(root, priorSecurity, workspaceSID, tempSID))
	}
	return restoreErr
}

func removeWorkspaceCapability(handle windows.Handle, original string, workspaceSID, tempSID *windows.SID) error {
	current, err := workspaceSecurityDescriptor(handle)
	if err != nil {
		return err
	}
	dacl, _, err := current.DACL()
	if err != nil {
		return err
	}
	changedWorkspace, err := stripWorkspaceCapability(dacl, workspaceSID)
	if err != nil {
		return err
	}
	changedTemp, err := stripWorkspaceCapability(dacl, tempSID)
	if err != nil || !changedWorkspace && !changedTemp {
		return err
	}
	// Existing objects regain their original integrity label. New objects
	// relinquish any Low label inherited from the temporary grant.
	var label *windows.ACL
	if original != "" {
		prior, err := windows.SecurityDescriptorFromString(original)
		if err != nil {
			return err
		}
		label, err = workspaceLabel(prior)
		if err != nil {
			return err
		}
	}
	return applyWorkspaceSecurity(handle, dacl, label)
}

func removeWorkspaceDescendantCapabilities(root string, priorSecurity map[workspaceFileID]string, workspaceSID, tempSID *windows.SID) error {
	var cleanupErr error
	walkErr := walkWorkspaceWithoutFollowing(root, windows.MAXIMUM_ALLOWED, func(path string, h windows.Handle, info windows.ByHandleFileInformation) error {
		if err := removeWorkspaceCapability(h, priorSecurity[workspaceIdentity(info)], workspaceSID, tempSID); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("revoke %s: %w", path, err))
		}
		return nil
	})
	return errors.Join(walkErr, cleanupErr)
}

func removeWorkspaceTemp(root string) error {
	var paths []string
	err := walkWorkspaceWithoutFollowing(root, windows.FILE_READ_ATTRIBUTES, func(path string, _ windows.Handle, _ windows.ByHandleFileInformation) error {
		paths = append(paths, path)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) && len(paths) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	for index := len(paths) - 1; index >= 0; index-- {
		if err := removeWorkspaceState(paths[index]); err != nil {
			return err
		}
	}
	return nil
}

func walkWorkspaceWithoutFollowing(root string, access uint32, visit func(string, windows.Handle, windows.ByHandleFileInformation) error) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		h, info, err := openWorkspaceObject(path, access, workspaceReadShare)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		canonical, err := workspaceFinalPath(h)
		if err != nil {
			return err
		}
		if !workspacePathWithin(root, canonical) {
			return fmt.Errorf("cannot recover redirected sandbox path %s", path)
		}
		if err := visit(path, h, info); err != nil {
			return err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && entry.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
}

func removeWorkspaceState(path string) error {
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
