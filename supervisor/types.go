// Package supervisor owns Docker sandboxes on the local host. Call Initialize
// before Start, and Shutdown when the embedding process exits.
package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

var (
	ErrNotReady    = errors.New("supervisor is not initialized")
	ErrNotFound    = errors.New("sandbox is not registered")
	ErrBusy        = errors.New("sandbox already has an active command")
	ErrUnavailable = errors.New("sandbox is not running")
)

type State string

const (
	Starting      State = "starting"
	Running       State = "running"
	Stopping      State = "stopping"
	Stopped       State = "stopped"
	Closing       State = "closing"
	CleanupFailed State = "cleanup_failed"
	Closed        State = "closed"
)

type Config struct {
	OwnerID        string
	DockerHost     string // Empty uses DOCKER_HOST, then the local Unix socket.
	StateDir       string
	StartupTimeout time.Duration // Default: 30 seconds.
	StopGrace      time.Duration // Default: 10 seconds.
	CleanupTimeout time.Duration // Default: 30 seconds.
	Logger         *slog.Logger
}

type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

type SandboxSpec struct {
	Image       string
	Workspace   string
	Mounts      []Mount
	Env         []string // Explicit KEY=VALUE entries; never read from a host file.
	CPUs        float64
	MemoryBytes int64
	PidsLimit   int64 // Defaults to 2048.
}

type ExecSpec struct {
	Args    []string
	WorkDir string
	Env     []string
}

type SandboxInfo struct {
	CID          string
	State        State
	CleanupError string
}

// SandboxHandle has immutable identity. State is read through Lookup or List.
type SandboxHandle struct {
	CID        string
	opMu       sync.Mutex
	mu         sync.Mutex
	state      State
	cleanupErr error
	active     *ExecSession
}

type ExecResult struct{ ExitCode int }

// ExecSession owns the attached Docker connection. Drain both output readers;
// their bounded pipes apply backpressure to the command.
type ExecSession struct {
	Stdin          io.WriteCloser
	Stdout         io.ReadCloser
	Stderr         io.ReadCloser
	closeTransport func()
	done           chan struct{}
	mu             sync.Mutex
	result         ExecResult
	err            error
}

func (e *ExecSession) Wait(ctx context.Context) (ExecResult, error) {
	select {
	case <-e.done:
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.result, e.err
	case <-ctx.Done():
		return ExecResult{}, ctx.Err()
	}
}
