package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

type stdinWriter struct {
	io.Writer
	closeWrite func() error
	mu         sync.Mutex
	closed     bool
}

func (w *stdinWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	return w.Writer.Write(p)
}

func (w *stdinWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.closeWrite()
}

func (s *Supervisor) Exec(ctx context.Context, cid string, spec ExecSpec) (*ExecSession, error) {
	if len(spec.Args) == 0 || spec.Args[0] == "" {
		return nil, errors.New("command arguments are required")
	}
	if err := validEnv(spec.Env); err != nil {
		return nil, err
	}
	h, err := s.get(cid)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	if h.state != Running {
		h.mu.Unlock()
		return nil, ErrUnavailable
	}
	if h.active != nil {
		h.mu.Unlock()
		return nil, ErrBusy
	}
	// A placeholder reserves the sole exec slot through creation and attach.
	placeholder := &ExecSession{closeTransport: func() {}}
	h.active = placeholder
	h.mu.Unlock()
	release := func() {
		h.mu.Lock()
		if h.active == placeholder {
			h.active = nil
		}
		h.mu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	created, err := s.docker.ExecCreate(ctx, cid, client.ExecCreateOptions{
		Cmd: append([]string(nil), spec.Args...), WorkingDir: spec.WorkDir,
		Env: append([]string(nil), spec.Env...), AttachStdin: true, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		release()
		return nil, s.closeAfterExecError(cid, err)
	}
	attached, err := s.docker.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		release()
		return nil, s.closeAfterExecError(cid, err)
	}
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	e := &ExecSession{Stdout: stdoutR, Stderr: stderrR, done: make(chan struct{})}
	e.Stdin = &stdinWriter{Writer: attached.Conn, closeWrite: attached.CloseWrite}
	var abortOnce sync.Once
	e.closeTransport = func() { abortOnce.Do(func() { attached.Close(); stdoutR.Close(); stderrR.Close() }) }
	h.mu.Lock()
	if h.state != Running {
		h.active = nil
		h.mu.Unlock()
		e.closeTransport()
		return nil, ErrUnavailable
	}
	h.active = e
	h.mu.Unlock()
	go s.runExec(ctx, h, created.ID, e, &attached, stdoutW, stderrW)
	return e, nil
}

func (s *Supervisor) closeAfterExecError(cid string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.config.CleanupTimeout)
	defer cancel()
	return errors.Join(cause, s.Close(ctx, cid))
}

func (s *Supervisor) runExec(ctx context.Context, h *SandboxHandle, execID string, e *ExecSession, attached *client.ExecAttachResult, stdoutW, stderrW *io.PipeWriter) {
	cleanupDone := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			c, cancel := context.WithTimeout(context.Background(), s.config.CleanupTimeout)
			cleanupDone <- s.Close(c, h.CID)
			cancel()
		case <-e.done:
		}
	}()
	_, streamErr := stdcopy.StdCopy(stdoutW, stderrW, attached.Reader)
	stdoutW.CloseWithError(streamErr)
	stderrW.CloseWithError(streamErr)
	attached.Close()
	var result ExecResult
	var finalErr error
	if ctx.Err() != nil {
		finalErr = errors.Join(ctx.Err(), <-cleanupDone)
	} else if streamErr != nil {
		h.mu.Lock()
		state := h.state
		h.mu.Unlock()
		if state == Stopping || state == Stopped {
			finalErr = ErrUnavailable
		} else {
			finalErr = s.closeAfterExecError(h.CID, fmt.Errorf("exec stream: %w", streamErr))
		}
	} else {
		// Docker may finish sending output just before its exit state is visible.
		inspectCtx, cancel := context.WithTimeout(context.Background(), s.config.CleanupTimeout)
		for {
			status, err := s.docker.ExecInspect(inspectCtx, execID, client.ExecInspectOptions{})
			if err != nil {
				finalErr = s.closeAfterExecError(h.CID, err)
				break
			}
			if !status.Running {
				result.ExitCode = status.ExitCode
				break
			}
			select {
			case <-inspectCtx.Done():
				finalErr = s.closeAfterExecError(h.CID, inspectCtx.Err())
			case <-time.After(25 * time.Millisecond):
			}
			if finalErr != nil {
				break
			}
		}
		cancel()
	}
	h.mu.Lock()
	if h.active == e {
		h.active = nil
	}
	h.mu.Unlock()
	e.mu.Lock()
	e.result = result
	e.err = finalErr
	e.mu.Unlock()
	close(e.done)
}
