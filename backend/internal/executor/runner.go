// internal/executor/runner.go — Code Executor
//
// Manages a temporary workspace directory per session, writes AI-generated
// Go source files, and executes them in a sandboxed subprocess.
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
// It also writes a go.mod file if none exists (so the code is compilable).
func WriteToWorkspace(sessionID, filename, code string) error {
	dir, err := ensureWorkspace(sessionID)
	if err != nil {
		return err
	}

	// Sanitize filename
	filename = filepath.Base(filename)
	if !strings.HasSuffix(filename, ".go") {
		filename += ".go"
	}

	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, []byte(code), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filePath, err)
	}

	// Ensure a go.mod exists so `go run` works without GOPATH shenanigans
	goModPath := filepath.Join(dir, "go.mod")
	if _, err := os.Stat(goModPath); os.IsNotExist(err) {
		goMod := "module proviber-workspace\n\ngo 1.21\n"
		if writeErr := os.WriteFile(goModPath, []byte(goMod), 0o644); writeErr != nil {
			return fmt.Errorf("write go.mod: %w", writeErr)
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

// ListWorkspaceFiles returns all .go files in the session workspace.
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
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			files = append(files, e.Name())
		}
	}
	return files, nil
}

// CleanWorkspace removes a session's workspace directory.
// Call this after the session ends to reclaim disk space.
func CleanWorkspace(sessionID string) error {
	dir := workspacePath(sessionID)
	return os.RemoveAll(dir)
}

// ─────────────────────────────────────────────────────────────
// CODE EXECUTION
// ─────────────────────────────────────────────────────────────

// RunGoCode compiles and runs the Go file at workspace/<sessionID>/<filename>.
//
// Returns:
//   - stdout: program's standard output
//   - stderr: compiler errors or runtime panics
//   - err:    non-nil if the process exited with a non-zero code
//
// The subprocess is killed after executionTimeout to prevent hangs.
func RunGoCode(sessionID, filename string) (stdout, stderr string, err error) {
	dir := workspacePath(sessionID)
	filename = filepath.Base(filename)
	filePath := filepath.Join(dir, filename)

	// Verify the file exists before attempting to run
	if _, statErr := os.Stat(filePath); os.IsNotExist(statErr) {
		return "", "", fmt.Errorf("file not found: %s", filePath)
	}

	// Use a context with timeout to kill runaway processes
	ctx, cancel := context.WithTimeout(context.Background(), executionTimeout)
	defer cancel()

	// `go run <file>` — compiles and runs in one step.
	// We run in the workspace dir so relative imports/files work.
	cmd := exec.CommandContext(ctx, "go", "run", filename) //nolint:gosec
	cmd.Dir = dir

	// Capture stdout and stderr separately
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// Set a restricted environment: only pass through PATH and GOPATH.
	// This prevents leaking host env vars into the subprocess.
	cmd.Env = []string{
		"HOME=/tmp",
		"PATH=/usr/local/go/bin:/usr/bin:/bin",
		"GOPATH=/tmp/gopath",
		"GOCACHE=/tmp/gocache",
		"GOMODCACHE=/tmp/gomodcache",
		"GOTMPDIR=/tmp",
	}

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
		// Exit error — stderr will contain the reason (compile error or panic)
		return stdoutStr, stderrStr, runErr
	}

	return stdoutStr, stderrStr, nil
}

// ─────────────────────────────────────────────────────────────
// BUILD-ONLY (compile check without running)
// ─────────────────────────────────────────────────────────────

// BuildGoCode runs `go build` without executing. Useful for large programs
// where running might have side effects.
func BuildGoCode(sessionID, filename string) (stderr string, err error) {
	dir := workspacePath(sessionID)
	filename = filepath.Base(filename)

	ctx, cancel := context.WithTimeout(context.Background(), executionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "build", "-o", "/dev/null", filename) //nolint:gosec
	cmd.Dir = dir
	cmd.Env = []string{
		"HOME=/tmp",
		"PATH=/usr/local/go/bin:/usr/bin:/bin",
		"GOPATH=/tmp/gopath",
		"GOCACHE=/tmp/gocache",
		"GOMODCACHE=/tmp/gomodcache",
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()
	return strings.TrimSpace(stderrBuf.String()), runErr
}
