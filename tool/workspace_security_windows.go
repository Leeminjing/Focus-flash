package tool

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	workspaceSecurity    = windows.DACL_SECURITY_INFORMATION | windows.LABEL_SECURITY_INFORMATION
	workspaceOpenFlags   = windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT
	workspaceReadShare   = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	workspaceFullShare   = workspaceReadShare | windows.FILE_SHARE_DELETE
	workspaceDeleteChild = 0x0040
	workspaceLabelACE    = 0x11
	workspaceNoWriteUp   = 0x1
)

var openWorkspaceFileByID = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenFileById")
var deleteWorkspaceACE = windows.NewLazySystemDLL("advapi32.dll").NewProc("DeleteAce")

type workspaceFileID struct {
	Volume uint32
	Index  uint64
}

func workspaceIdentity(info windows.ByHandleFileInformation) workspaceFileID {
	return workspaceFileID{info.VolumeSerialNumber, uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)}
}

func openWorkspaceObject(path string, access, share uint32) (windows.Handle, windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, info, err
	}
	handle, err := windows.CreateFile(name, access, share, nil, windows.OPEN_EXISTING, workspaceOpenFlags, 0)
	if err != nil {
		return 0, info, err
	}
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		windows.CloseHandle(handle)
		return 0, info, err
	}
	return handle, info, nil
}

func openWorkspaceObjectByID(volume windows.Handle, id workspaceFileID) (windows.Handle, windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	// FILE_ID_DESCRIPTOR's union is 16 bytes, including when a 64-bit NTFS ID is used.
	descriptor := struct {
		Size uint32
		Type uint32
		ID   uint64
		Pad  uint64
	}{Size: 24, ID: id.Index}
	value, _, err := openWorkspaceFileByID.Call(uintptr(volume), uintptr(unsafe.Pointer(&descriptor)),
		windows.MAXIMUM_ALLOWED, workspaceReadShare, 0, workspaceOpenFlags)
	handle := windows.Handle(value)
	if handle == windows.InvalidHandle {
		return 0, info, err
	}
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		windows.CloseHandle(handle)
		return 0, info, err
	}
	if workspaceIdentity(info) != id {
		windows.CloseHandle(handle)
		return 0, info, errors.New("workspace file identity changed")
	}
	return handle, info, nil
}

func workspaceFinalPath(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 512)
	for {
		size, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if size < uint32(len(buffer)) {
			path := windows.UTF16ToString(buffer[:size])
			if !strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\?\UNC\`) {
				return "", errors.New("workspace must have a local drive path")
			}
			return filepath.Clean(strings.TrimPrefix(path, `\\?\`)), nil
		}
		buffer = make([]uint16, size+1)
	}
}

func workspacePathWithin(root, path string) bool {
	root = strings.ToUpper(filepath.Clean(root))
	path = strings.ToUpper(filepath.Clean(path))
	return root == path || strings.HasPrefix(path, strings.TrimRight(root, `\`)+`\`)
}

func validateWorkspaceVolume(path string, handle windows.Handle) error {
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' {
		return errors.New("workspace must be on a fixed local NTFS drive")
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return err
	}
	if windows.GetDriveType(root) != windows.DRIVE_FIXED {
		return errors.New("workspace must be on a fixed local NTFS drive")
	}
	var fsName [16]uint16
	err = windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, nil, &fsName[0], uint32(len(fsName)))
	if err != nil {
		return err
	}
	if windows.UTF16ToString(fsName[:]) != "NTFS" {
		return errors.New("workspace must be formatted as NTFS")
	}
	return nil
}

func workspaceSecurityDescriptor(handle windows.Handle) (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, workspaceSecurity)
}

func applyWorkspaceSecurity(handle windows.Handle, dacl *windows.ACL, sacl *windows.ACL) error {
	// MAXIMUM_ALLOWED handles opt out of automatic recursive propagation. The
	// caller visits and journals each existing object before changing it.
	return windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, workspaceSecurity, nil, nil, dacl, sacl)
}

func workspaceLabel(descriptor *windows.SECURITY_DESCRIPTOR) (*windows.ACL, error) {
	sacl, _, err := descriptor.SACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		return nil, nil
	}
	return sacl, err
}

func validateWorkspaceLabel(descriptor *windows.SECURITY_DESCRIPTOR) error {
	label, err := workspaceLabel(descriptor)
	if err != nil || label == nil {
		return err
	}
	for index := uint32(0); index < uint32(label.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(label, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == workspaceLabelACE && ace.Mask&^workspaceNoWriteUp != 0 {
			return errors.New("workspace has an integrity policy restricting reads or execution")
		}
	}
	return nil
}

func stripWorkspaceCapability(dacl *windows.ACL, sid *windows.SID) (bool, error) {
	if dacl == nil {
		return false, errors.New("a null workspace DACL cannot be confined")
	}
	changed := false
	for index := int(dacl.AceCount) - 1; index >= 0; index-- {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return false, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
			continue
		}
		// SetEntriesInAcl's REVOKE_ACCESS leaves inherited ACEs in place.
		if result, _, err := deleteWorkspaceACE.Call(uintptr(unsafe.Pointer(dacl)), uintptr(index)); result == 0 {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

func grantWorkspaceSecurity(handle windows.Handle, descriptor *windows.SECURITY_DESCRIPTOR, sid *windows.SID, directory, root bool) error {
	priorDACL, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if priorDACL == nil {
		return errors.New("a null workspace DACL cannot be confined")
	}
	permissions := windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.FILE_GENERIC_EXECUTE | windows.DELETE)
	if directory {
		permissions |= workspaceDeleteChild
	}
	grant := windows.EXPLICIT_ACCESS{
		AccessPermissions: permissions,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid)},
	}
	entries := []windows.EXPLICIT_ACCESS{grant}
	if directory {
		entries[0].Inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
		if root {
			// Descendants may be moved and removed. The root remains available
			// for recovery, and its parent is outside the granted tree.
			entries[0].Inheritance |= windows.INHERIT_ONLY
			grant.AccessPermissions &^= windows.DELETE
			entries = append(entries, grant)
		}
	}
	dacl, err := windows.ACLFromEntries(entries, priorDACL)
	runtime.KeepAlive(sid)
	if err != nil {
		return err
	}
	label := "S:(ML;;NW;;;LW)"
	if directory {
		label = "S:(ML;OICI;NW;;;LW)"
	}
	low, err := windows.SecurityDescriptorFromString(label)
	if err != nil {
		return err
	}
	sacl, _, err := low.SACL()
	if err != nil {
		return err
	}
	if err := applyWorkspaceSecurity(handle, dacl, sacl); err != nil {
		return fmt.Errorf("set workspace DACL and integrity label: %w", err)
	}
	return nil
}
