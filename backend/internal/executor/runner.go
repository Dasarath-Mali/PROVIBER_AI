// internal/executor/runner.go — Code Executor
//
// Manages a temporary workspace directory per session, writes AI-generated
// source files, and executes them in a sandboxed subprocess.
//
// Security note:
//   Running arbitrary AI-generated code is inherently risky.
//   In production, wrap this in a proper container/sandbox (e.g., gVisor, Firecracker).
//   For a free-tier demo, we rely on OS-level process isolation and timeouts.

package executor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────
// CONSTANTS
// ─────────────────────────────────────────────────────────────

const (
	// Root directory under which all session workspaces live.
	workspaceRoot = "/tmp/proviber-workspaces"

	// Maximum execution time before we kill the subprocess.
	// Keeps runaway loops from hanging Render's free instance.
	executionTimeout = 15 * time.Second

	// Maximum size of stdout/stderr we'll capture (16 KB).
	maxOutputBytes = 16 * 1024
)

// ─────────────────────────────────────────────────────────────
// WORKSPACE MANAGEMENT
// ─────────────────────────────────────────────────────────────

// workspacePath returns the directory for a given session.
func workspacePath(sessionID string) string {
	// Sanitize sessionID to prevent path traversal
	clean := filepath.Base(sessionID)
	if clean == "." || clean == "/" {
		clean = "unknown"
	}
	return filepath.Join(workspaceRoot, clean)
}

// ensureWorkspace creates the session workspace directory if it doesn't exist.
func ensureWorkspace(sessionID string) (string, error) {
	dir := workspacePath(sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create workspace dir %s: %w", dir, err)
	}
	return dir, nil
}

// WriteToWorkspace writes code to workspace/<sessionID>/<filename>.
// If it's a Go file, it also writes a go.mod file.
func WriteToWorkspace(sessionID, filename, code string) error {
	dir, err := ensureWorkspace(sessionID)
	if err != nil {
		return err
	}

	// Sanitize filename
	filename = filepath.Base(filename)
	ext := strings.ToLower(filepath.Ext(filename))

	// If the AI somehow didn't give it an extension, default to .txt
	if ext == "" {
		filename += ".txt"
		ext = ".txt"
	}

	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, []byte(code), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filePath, err)
	}

	// Only create go.mod if we are dealing with Go code
	if ext == ".go" {
		goModPath := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(goModPath); os.IsNotExist(err) {
			goMod := "module proviber-workspace\n\ngo 1.21\n"
			if writeErr := os.WriteFile(goModPath, []byte(goMod), 0o644); writeErr != nil {
				return fmt.Errorf("write go.mod: %w", writeErr)
			}
		}
	}

	return nil
}

// ReadWorkspaceFile reads the content of a file in the session workspace.
func ReadWorkspaceFile(sessionID, filename string) (string, error) {
	dir := workspacePath(sessionID)
	filename = filepath.Base(filename) // prevent traversal
	content, err := os.ReadFile(filepath.Join(dir, filename))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", filename, err)
	}
	return string(content), nil
}

// ListWorkspaceFiles returns all supported source files in the session workspace.
func ListWorkspaceFiles(sessionID string) ([]string, error) {
	dir := workspacePath(sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("list workspace: %w", err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() || e.Name() == "go.mod" {
			continue // Skip directories and the hidden go.mod file
		}

		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".go" || ext == ".py" || ext == ".js" || ext == ".txt" {
			files = append(files, e.Name())
		}
	}
	return files, nil
}

// CleanWorkspace removes a session's workspace directory.
func CleanWorkspace(sessionID string) error {
	dir := workspacePath(sessionID)
	return os.RemoveAll(dir)
}

// ─────────────────────────────────────────────────────────────
// CODE EXECUTION (POLYGLOT)
// ─────────────────────────────────────────────────────────────

// RunCode runs the file at workspace/<sessionID>/<filename> based on its extension.
//
// Returns:
//   - stdout: program's standard output
//   - stderr: compiler/interpreter errors or runtime panics
//   - err:    non-nil if the process exited with a non-zero code
func RunCode(sessionID, filename string) (stdout, stderr string, err error) {
	dir := workspacePath(sessionID)
	filename = filepath.Base(filename)
	filePath := filepath.Join(dir, filename)

	// Verify the file exists before attempting to run
	if _, statErr := os.Stat(filePath); os.IsNotExist(statErr) {
		return "", "", fmt.Errorf("file not found: %s", filePath)
	}

	ext := strings.ToLower(filepath.Ext(filename))
	ctx, cancel := context.WithTimeout(context.Background(), executionTimeout)
	defer cancel()

	var cmd *exec.Cmd

	// Choose the correct runner based on file extension
	switch ext {
	case ".go":
		cmd = exec.CommandContext(ctx, "go", "run", filename)
		cmd.Env = []string{
			"HOME=/tmp",
			"PATH=/usr/local/go/bin:/usr/bin:/bin",
			"GOPATH=/tmp/gopath",
			"GOCACHE=/tmp/gocache",
			"GOMODCACHE=/tmp/gomodcache",
			"GOTMPDIR=/tmp",
		}
	case ".py":
		cmd = exec.CommandContext(ctx, "python3", filename)
		cmd.Env = []string{
			"HOME=/tmp",
			"PATH=/usr/bin:/bin:/usr/local/bin",
		}
	case ".js":
		cmd = exec.CommandContext(ctx, "node", filename)
		cmd.Env = []string{
			"HOME=/tmp",
			"PATH=/usr/bin:/bin:/usr/local/bin",
		}
	default:
		return "", "", fmt.Errorf("unsupported language extension: %s", ext)
	}

	cmd.Dir = dir

	// Capture stdout and stderr separately
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()

	// Truncate output to maxOutputBytes
	rawStdout := stdoutBuf.Bytes()
	rawStderr := stderrBuf.Bytes()
	if len(rawStdout) > maxOutputBytes {
		rawStdout = append(rawStdout[:maxOutputBytes], []byte("\n... (output truncated)")...)
	}
	if len(rawStderr) > maxOutputBytes {
		rawStderr = append(rawStderr[:maxOutputBytes], []byte("\n... (output truncated)")...)
	}

	stdoutStr := strings.TrimSpace(string(rawStdout))
	stderrStr := strings.TrimSpace(string(rawStderr))

	// Context deadline exceeded = our timeout fired
	if ctx.Err() == context.DeadlineExceeded {
		return stdoutStr, "execution timed out after 15 seconds", ctx.Err()
	}

	if runErr != nil {
		return stdoutStr, stderrStr, runErr
	}

	return stdoutStr, stderrStr, nil
}
