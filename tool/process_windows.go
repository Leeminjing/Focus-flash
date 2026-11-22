package tool

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	disableMaxPrivilege     = 0x1
	luaToken                = 0x4
	writeRestricted         = 0x8
	mandatoryNoWriteUp      = 0x1
	processAttributeJobs    = 0x0002000D
	maxProcessCommandSize   = 32767 // UTF-16 code units, including the terminator.
	statusDLLInitFailed     = 0xC0000142
	statusHostFXRLoadFailed = 0x80008082
)

var createRestrictedToken = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")
var errWindowsPowerShellStartup = errors.New("PowerShell 7 could not initialize")

type windowsProcess struct {
	Stdin  *os.File
	Output *os.File

	process     windows.Handle
	job         windows.Handle
	jobMu       sync.Mutex
	wait        sync.Once
	code        int
	waitErr     error
	treeStopped bool   // Available after Wait, including when the command or shell failed.
	mode        string // Set by the PowerShell launcher to contextualize startup failures.
}

func trustedPowerShell(workspace string) (string, error) {
	if workspace != "" {
		root, err := filepath.EvalSymlinks(workspace)
		if err != nil {
			return "", fmt.Errorf("resolve workspace for PowerShell 7: %w", err)
		}
		workspace = root
	}
	var candidates []string
	if programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0); err == nil {
		candidates = append(candidates, filepath.Join(programFiles, "PowerShell", "7", "pwsh.exe"))
	}
	if path, err := exec.LookPath("pwsh.exe"); err == nil && filepath.IsAbs(path) {
		candidates = append(candidates, path)
	}
	for _, candidate := range candidates {
		path, err := windows.UTF16PtrFromString(candidate)
		if err != nil {
			continue
		}
		handle, err := windows.CreateFile(path, windows.FILE_READ_ATTRIBUTES, workspaceFullShare, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			continue
		}
		executable, err := workspaceFinalPath(handle)
		windows.CloseHandle(handle)
		if err != nil {
			continue
		}
		installDir := filepath.Dir(executable)
		if workspace != "" && (workspacePathWithin(workspace, executable) || workspacePathWithin(installDir, workspace)) {
			continue
		}
		if hasPowerShell7Version(executable) {
			return executable, nil
		}
	}
	return "", errors.New("PowerShell 7 (pwsh.exe) is required in the standard installation directory or on the host PATH outside the workspace")
}

func hasPowerShell7Version(path string) bool {
	var handle windows.Handle
	size, err := windows.GetFileVersionInfoSize(path, &handle)
	if err != nil || size == 0 {
		return false
	}
	info := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&info[0])); err != nil {
		return false
	}
	var version *windows.VS_FIXEDFILEINFO
	var length uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&info[0]), `\`, unsafe.Pointer(&version), &length); err != nil {
		return false
	}
	return version != nil && uintptr(length) >= unsafe.Sizeof(*version) && version.ProductVersionMS>>16 == 7
}

func startWindowsProcess(path, mode, workdir, script string, workspaceSID, tempSID *windows.SID, tempPath string) (*windowsProcess, error) {
	command, err := powerShellCommandLine(path, script)
	if err != nil {
		return nil, err
	}
	process, err := startWindowsExecutable(mode, workdir, path, command, workspaceSID, tempSID, tempPath)
	if err != nil {
		return nil, err
	}
	process.mode = mode
	return process, nil
}

func startWindowsExecutable(mode, workdir, path string, command *uint16, workspaceSID, tempSID *windows.SID, tempPath string) (*windowsProcess, error) {
	executable, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	directory, err := windows.UTF16PtrFromString(workdir)
	if err != nil {
		return nil, err
	}

	token, err := windowsExecutionToken(mode, workspaceSID, tempSID, tempPath)
	if err != nil {
		return nil, err
	}
	if token != 0 {
		defer token.Close()
	}
	var env *uint16
	if mode == "workspace-write" {
		block, err := privateTempEnvironment(tempPath)
		if err != nil {
			return nil, err
		}
		env = &block[0]
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create Windows job: %w", err)
	}
	started := false
	defer func() {
		if !started {
			windows.CloseHandle(job)
		}
	}()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, fmt.Errorf("limit Windows job: %w", err)
	}

	in, out, childHandles, err := processPipes()
	if err != nil {
		return nil, fmt.Errorf("create process pipes: %w", err)
	}
	defer func() {
		for _, handle := range childHandles {
			windows.CloseHandle(handle)
		}
		if !started {
			in.Close()
			out.Close()
		}
	}()

	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, fmt.Errorf("allocate process attribute list: %w", err)
	}
	defer attributes.Delete()
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&childHandles[0]), unsafe.Sizeof(childHandles)); err != nil {
		return nil, fmt.Errorf("limit inherited handles: %w", err)
	}
	if err := attributes.Update(processAttributeJobs, unsafe.Pointer(&job), unsafe.Sizeof(job)); err != nil {
		return nil, fmt.Errorf("assign process to job at startup: %w", err)
	}

	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:        uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags:     windows.STARTF_USESTDHANDLES,
			StdInput:  childHandles[0],
			StdOutput: childHandles[1],
			StdErr:    childHandles[1],
		},
		ProcThreadAttributeList: attributes.List(),
	}
	var noninteractiveDesktop uint16
	if token != 0 {
		startup.Desktop = &noninteractiveDesktop
	}
	const flags = windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_NO_WINDOW | windows.CREATE_UNICODE_ENVIRONMENT
	var process windows.ProcessInformation
	if token == 0 {
		err = windows.CreateProcess(executable, command, nil, nil, true, flags, env, directory, &startup.StartupInfo, &process)
	} else {
		err = windows.CreateProcessAsUser(token, executable, command, nil, nil, true, flags, env, directory, &startup.StartupInfo, &process)
	}
	if err != nil {
		return nil, fmt.Errorf("start %s in %s: %w", filepath.Base(path), mode, err)
	}
	windows.CloseHandle(process.Thread)
	started = true
	return &windowsProcess{Stdin: in, Output: out, process: process.Process, job: job}, nil
}

func powerShellCommandLine(path, script string) (*uint16, error) {
	units := utf16.Encode([]rune(script))
	bytes := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(bytes[i*2:], unit)
	}
	encoded := base64.StdEncoding.EncodeToString(bytes)
	// Only the encoding of the command is model-controlled. It is a single
	// argument and cannot substitute the executable or change startup flags.
	line := windows.ComposeCommandLine([]string{path, "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded})
	command, err := windows.UTF16FromString(line)
	if err != nil {
		return nil, err
	}
	if len(command) > maxProcessCommandSize {
		return nil, errors.New("PowerShell command exceeds the Windows command-line limit")
	}
	return &command[0], nil
}

func privateTempEnvironment(tempPath string) ([]uint16, error) {
	env := os.Environ()
	env = slices.DeleteFunc(env, func(entry string) bool {
		key, _, _ := strings.Cut(entry, "=")
		return strings.EqualFold(key, "TEMP") || strings.EqualFold(key, "TMP")
	})
	env = append(env, "TEMP="+tempPath, "TMP="+tempPath)
	slices.SortFunc(env, func(a, b string) int {
		return strings.Compare(strings.ToUpper(a), strings.ToUpper(b))
	})
	block := make([]uint16, 0)
	for _, entry := range env {
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, fmt.Errorf("encode PowerShell environment: %w", err)
		}
		block = append(block, encoded...)
	}
	return append(block, 0), nil
}

func windowsExecutionToken(mode string, workspaceSID, tempSID *windows.SID, tempPath string) (windows.Token, error) {
	var restrictions []windows.SIDAndAttributes
	switch mode {
	case "danger-full-access":
		return 0, nil
	case "read-only":
	case "workspace-write":
		if workspaceSID == nil || tempSID == nil || windows.EqualSid(workspaceSID, tempSID) || !filepath.IsAbs(tempPath) {
			return 0, errors.New("workspace-write requires distinct workspace and private temporary directory grants")
		}
		restrictions = []windows.SIDAndAttributes{{Sid: workspaceSID}, {Sid: tempSID}}
	default:
		return 0, fmt.Errorf("unknown Windows execution mode %q", mode)
	}
	// A fresh SID grants access only to this process's default kernel objects.
	// It keeps the read-only list nonempty without authorizing any existing file.
	processSID, err := newWindowsCapability()
	if err != nil {
		return 0, fmt.Errorf("create process SID: %w", err)
	}
	restrictions = append(restrictions, windows.SIDAndAttributes{Sid: processSID})

	var parent windows.Token
	const tokenAccess = windows.TOKEN_ASSIGN_PRIMARY | windows.TOKEN_DUPLICATE | windows.TOKEN_QUERY | windows.TOKEN_ADJUST_DEFAULT
	if err := windows.OpenProcessToken(windows.CurrentProcess(), tokenAccess, &parent); err != nil {
		return 0, fmt.Errorf("open host token: %w", err)
	}
	defer parent.Close()
	// Deriving from another restricted token would take the intersection of
	// the two SID lists and could discard this task's intended restrictions.
	if restricted, err := parent.IsRestricted(); err != nil {
		return 0, fmt.Errorf("inspect host token: %w", err)
	} else if restricted {
		return 0, errors.New("cannot provision Windows sandbox from an already restricted host token")
	}
	var token windows.Token
	result, _, err := createRestrictedToken.Call(uintptr(parent), disableMaxPrivilege|luaToken|writeRestricted,
		0, 0, 0, 0, uintptr(len(restrictions)), uintptr(unsafe.Pointer(&restrictions[0])), uintptr(unsafe.Pointer(&token)))
	runtime.KeepAlive(restrictions)
	if result == 0 {
		return 0, fmt.Errorf("create Windows restricted token: %w", err)
	}
	if err := setLowIntegrity(token); err != nil {
		token.Close()
		return 0, err
	}
	if err := setRestrictedDefaultDACL(token, processSID); err != nil {
		token.Close()
		return 0, err
	}
	return token, nil
}

func setRestrictedDefaultDACL(token windows.Token, restriction *windows.SID) error {
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("query restricted token user: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;%s)(A;;GA;;;%s)", user.User.Sid, restriction))
	if err != nil {
		return fmt.Errorf("build restricted token default DACL: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("decode restricted token default DACL: %w", err)
	}
	defaultDACL := struct{ ACL *windows.ACL }{dacl}
	if err := windows.SetTokenInformation(token, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&defaultDACL)), uint32(unsafe.Sizeof(defaultDACL))); err != nil {
		return fmt.Errorf("set restricted token default DACL: %w", err)
	}
	return nil
}

func setLowIntegrity(token windows.Token) error {
	low, err := windows.StringToSid("S-1-16-4096")
	if err != nil {
		return fmt.Errorf("create low integrity SID: %w", err)
	}
	label := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: low, Attributes: windows.SE_GROUP_INTEGRITY}}
	if err := windows.SetTokenInformation(token, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&label)), label.Size()); err != nil {
		return fmt.Errorf("set low token integrity: %w", err)
	}
	var policy, size uint32
	if err := windows.GetTokenInformation(token, windows.TokenMandatoryPolicy, (*byte)(unsafe.Pointer(&policy)), uint32(unsafe.Sizeof(policy)), &size); err != nil {
		return fmt.Errorf("check no-write-up token policy: %w", err)
	}
	if policy&mandatoryNoWriteUp == 0 {
		return errors.New("restricted token is missing the mandatory no-write-up policy")
	}
	return nil
}

func processPipes() (input, output *os.File, children [2]windows.Handle, err error) {
	childIn, parentIn, err := os.Pipe()
	if err != nil {
		return nil, nil, children, err
	}
	defer childIn.Close()
	parentOut, childOut, err := os.Pipe()
	if err != nil {
		parentIn.Close()
		return nil, nil, children, err
	}
	defer childOut.Close()
	for i, pipe := range []*os.File{childIn, childOut} {
		current := windows.CurrentProcess()
		if err := windows.DuplicateHandle(current, windows.Handle(pipe.Fd()), current, &children[i], 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			for _, handle := range children[:i] {
				windows.CloseHandle(handle)
			}
			parentIn.Close()
			parentOut.Close()
			return nil, nil, children, err
		}
	}
	return parentIn, parentOut, children, nil
}

func (p *windowsProcess) Kill() error {
	p.jobMu.Lock()
	defer p.jobMu.Unlock()
	if p.job == 0 {
		return nil
	}
	if err := windows.TerminateJobObject(p.job, 1); err != nil {
		return fmt.Errorf("terminate Windows job: %w", err)
	}
	return nil
}

// Wait reaps the shell and its remaining descendants. The pipe reader can
// drain output concurrently, including bytes buffered before termination.
func (p *windowsProcess) Wait() (int, error) {
	p.wait.Do(func() {
		var code uint32
		_, p.waitErr = windows.WaitForSingleObject(p.process, windows.INFINITE)
		if p.waitErr == nil {
			p.waitErr = windows.GetExitCodeProcess(p.process, &code)
		}
		if p.waitErr == nil && p.mode != "" && (code == statusDLLInitFailed || code == statusHostFXRLoadFailed) {
			p.waitErr = fmt.Errorf("%w in %s (exit status 0x%08X)", errWindowsPowerShellStartup, p.mode, code)
		}
		p.code = int(int32(code))
		p.waitErr = errors.Join(p.waitErr, windows.CloseHandle(p.process), p.reapJob())
	})
	return p.code, p.waitErr
}

func (p *windowsProcess) reapJob() error {
	p.jobMu.Lock()
	defer p.jobMu.Unlock()
	job := p.job
	p.job = 0
	err := waitForJobTermination(job)
	p.treeStopped = err == nil
	return errors.Join(err, windows.CloseHandle(job))
}

// JOB_OBJECT_BASIC_ACCOUNTING_INFORMATION includes the number of running
// processes. Check it before returning, so workspace grants can be reclaimed.
func waitForJobTermination(job windows.Handle) error {
	if err := windows.TerminateJobObject(job, 1); err != nil {
		return fmt.Errorf("terminate remaining Windows job processes: %w", err)
	}
	var counts struct {
		userTime, kernelTime, periodUserTime, periodKernelTime  int64
		pageFaults, totalProcesses, activeProcesses, terminated uint32
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&counts)), uint32(unsafe.Sizeof(counts)), nil)
		if err != nil {
			return fmt.Errorf("check remaining Windows job processes: %w", err)
		}
		if counts.activeProcesses == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("Windows job did not stop all processes")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
