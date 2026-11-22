package tool

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

//go:embed admission_parse_windows.ps1
var admissionParser string

var (
	parentPathSegment = regexp.MustCompile(`(^|[\\/ 	=:'"(,;])\.\.($|[\\/ 	'"),;])`)
	absolutePathRef   = regexp.MustCompile(`(?i)(^|[ 	\r\n='"(,;])([a-z]:|[\\/])`)
	providerPathRef   = regexp.MustCompile(`(?i)(^|[ 	\r\n=:'"(,;])(?:(?:hklm|hkcu|env|alias|variable|function|cert|wsman):|[a-z][a-z.]*::)`)
	pathAssignment    = regexp.MustCompile(`(?i)(^|[ \t\r\n;])(?:export[ \t]+|set[ \t]+)?path[ \t]*=`)
)

type powershellAdmission struct {
	Invalid     bool
	ChangesPath bool
	Values      []struct {
		Text       string
		Executable bool
	}
	Locations []struct {
		Target string
		Valid  bool
		Nested bool
	}
}

func validateShellCommand(ctx context.Context, powershell, command, workspace, workdir string, mode sandboxMode) error {
	if mode == sandboxFullAccess {
		return nil
	}
	workdir, err := validateWorkspacePath(workspace, workdir)
	if err != nil {
		return fmt.Errorf("shell working directory: %w", err)
	}
	directory, err := os.Stat(workdir)
	if err != nil {
		return fmt.Errorf("shell working directory: %w", err)
	}
	if !directory.IsDir() {
		return errors.New("shell working directory must be a directory")
	}

	parsed, err := parseShellAdmission(ctx, powershell, command)
	if err != nil {
		return err
	}
	if parsed.ChangesPath || pathAssignment.MatchString(command) {
		slog.WarnContext(ctx, "shell command changes PATH")
	}

	for _, location := range parsed.Locations {
		if location.Nested || !location.Valid || location.Target == "" ||
			strings.HasPrefix(location.Target, "~") || strings.ContainsAny(location.Target, "$`*?[]") ||
			location.Target == "-" || location.Target == "+" {
			return errors.New("shell directory change requires a literal workspace path outside a subexpression")
		}
		if _, err := validateLocalPath(workspace, workdir, location.Target); err != nil {
			return fmt.Errorf("shell directory change: %w", err)
		}
	}
	systemDirectory, err := windows.GetSystemDirectory()
	if err != nil {
		return fmt.Errorf("locate Windows system directory: %w", err)
	}
	for _, value := range parsed.Values {
		if err := validateShellLiteral(workspace, workdir, systemDirectory, value.Text, value.Executable); err != nil {
			return err
		}
	}
	return nil
}

func parseShellAdmission(ctx context.Context, powershell, command string) (powershellAdmission, error) {
	units := utf16.Encode([]rune(admissionParser))
	encodedScript := make([]byte, len(units)*2)
	for i, unit := range units {
		binary.LittleEndian.PutUint16(encodedScript[i*2:], unit)
	}
	parser := exec.CommandContext(ctx, powershell, "-NoLogo", "-NoProfile", "-NonInteractive",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(encodedScript))
	// Only the bundled parser is run. The command is read as data, and never
	// used as the parser's script or used to look up an executable.
	parser.Dir = filepath.Dir(powershell)
	parser.Stdin = strings.NewReader(base64.StdEncoding.EncodeToString([]byte(command)))
	output, err := parser.Output()
	var parsed powershellAdmission
	switch {
	case err != nil:
		return parsed, fmt.Errorf("start PowerShell command parser: %w", err)
	case json.Unmarshal(output, &parsed) != nil:
		return parsed, errors.New("invalid response from PowerShell command parser")
	case parsed.Invalid:
		return parsed, errors.New("invalid PowerShell command syntax")
	}
	return parsed, nil
}

func validateShellLiteral(workspace, workdir, systemDirectory, literal string, executable bool) error {
	if strings.Contains(strings.ToLower(literal), "file://") || parentPathSegment.MatchString(literal) {
		return errors.New("shell command refers to a file URL or a parent path segment")
	}
	// PATH can be updated with either an environment variable assignment or
	// the Env provider form. Other explicit providers are outside local paths.
	if strings.EqualFold(literal, "env:path") || strings.EqualFold(literal, `env:\path`) || strings.EqualFold(literal, "env:/path") {
		return nil
	}
	if providerPathRef.MatchString(literal) {
		return errors.New("shell command refers to a PowerShell provider path")
	}

	locations := absolutePathRef.FindAllStringSubmatchIndex(literal, -1)
	for index, match := range locations {
		end := len(literal)
		if index+1 < len(locations) {
			end = locations[index+1][0]
		}
		name := strings.TrimSpace(literal[match[4]:end])
		if delimiter := strings.IndexAny(name, "'\";\r\n"); delimiter >= 0 {
			name = name[:delimiter]
		}
		if err := validateShellPath(workspace, workdir, name); err != nil {
			if executable && name == literal && systemExecutable(systemDirectory, name) {
				continue
			}
			return fmt.Errorf("shell absolute path: %w", err)
		}
	}
	if len(locations) == 0 && strings.ContainsAny(literal, `\/`) && !strings.Contains(literal, "://") {
		name := literal
		if key, path, found := strings.Cut(literal, "="); found && strings.HasPrefix(key, "-") {
			name = path
		}
		// Dynamic references are evaluated by the process, under Windows
		// permissions. Check literal relative paths for existing links.
		if !strings.ContainsAny(name, "$`") {
			if err := validateShellPath(workspace, workdir, name); err != nil {
				return fmt.Errorf("shell relative path: %w", err)
			}
		}
	} else if len(locations) == 0 && literal != "" && !strings.ContainsAny(literal, `\/:*?"<>|$`) {
		// A plain name may also designate an existing file or directory.
		if _, err := os.Lstat(filepath.Join(workdir, literal)); err == nil {
			if _, err := validateLocalPath(workspace, workdir, literal); err != nil {
				return fmt.Errorf("shell relative path: %w", err)
			}
		}
	}
	return nil
}

func validateShellPath(workspace, workdir, path string) error {
	if !localPathSyntax(path) {
		return errors.New("path must be a local workspace path")
	}
	// Wildcards have no literal NTFS object. Check the existing prefix.
	if wildcard := strings.IndexAny(path, "*?"); wildcard >= 0 {
		if separator := strings.LastIndexAny(path[:wildcard], `\/`); separator >= 0 {
			path = path[:separator+1]
		} else {
			path = "."
		}
	}
	if filepath.Base(path) == "..." {
		path = filepath.Dir(path)
	}
	_, err := validateLocalPath(workspace, workdir, path)
	return err
}

func systemExecutable(systemDirectory, path string) bool {
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Ext(path), ".exe") {
		return false
	}
	resolved, err := validateWorkspacePath(systemDirectory, path)
	if err != nil {
		return false
	}
	file, err := os.Stat(resolved)
	return err == nil && file.Mode().IsRegular()
}

func validateWorkspacePath(workspace, path string) (string, error) {
	return validateLocalPath(workspace, workspace, path)
}

func validateLocalPath(workspace, base, path string) (string, error) {
	if !localPathSyntax(path) {
		return "", errors.New("path must be a local workspace path")
	}
	absolute := filepath.Clean(path)
	if !filepath.IsAbs(absolute) {
		absolute = filepath.Join(base, absolute)
	}
	if !pathWithin(workspace, absolute) {
		return "", errors.New("path is outside the workspace")
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("inspect workspace: %w", err)
	}
	// Validate the nearest existing ancestor, including an existing leaf.
	// A new path must be backed by an ancestor in the same physical workspace.
	for existing := absolute; pathWithin(workspace, existing); existing = filepath.Dir(existing) {
		_, statErr := os.Lstat(existing)
		switch {
		case statErr == nil:
			resolved, resolveErr := filepath.EvalSymlinks(existing)
			if resolveErr != nil {
				return "", fmt.Errorf("resolve path: %w", resolveErr)
			}
			if !pathWithin(root, resolved) {
				return "", errors.New("path resolves outside the workspace")
			}
			relative, err := filepath.Rel(root, resolved)
			if err != nil {
				return "", err
			}
			ancestor := resolved
			if relative != "." {
				for range strings.Split(relative, `\`) {
					ancestor = filepath.Dir(ancestor)
				}
			}
			ancestorInfo, err := os.Stat(ancestor)
			if err != nil {
				return "", fmt.Errorf("inspect workspace ancestor: %w", err)
			}
			// filepath.Rel folds case on Windows, but a directory may have
			// case-sensitive names. Verify the ancestor by file identity.
			if !os.SameFile(rootInfo, ancestorInfo) {
				return "", errors.New("path resolves outside the workspace")
			}
			return absolute, nil
		case !errors.Is(statErr, os.ErrNotExist):
			return "", fmt.Errorf("inspect path: %w", statErr)
		case existing == filepath.Dir(existing):
			return "", statErr
		}
	}
	return "", errors.New("path has no existing workspace ancestor")
}

func localPathSyntax(path string) bool {
	normalized := strings.ReplaceAll(path, "/", `\`)
	volume := filepath.VolumeName(normalized)
	return path != "" && !strings.HasPrefix(normalized, "~") && !strings.HasPrefix(normalized, `\`) &&
		(volume == "" || filepath.IsAbs(normalized)) && !providerPathRef.MatchString(normalized) &&
		!strings.Contains(strings.TrimPrefix(normalized, volume), ":") && !parentPathSegment.MatchString(normalized)
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, `..\`)
}
