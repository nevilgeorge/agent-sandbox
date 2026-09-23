package supervisor

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// Stop stops a sandbox but retains its handle. A stopped sandbox cannot be
// restarted by this package; Close removes it and its registry entry.
func (s *Supervisor) Stop(ctx context.Context, cid string) error {
	h, err := s.get(cid)
	if err != nil {
		return err
	}
	h.opMu.Lock()
	defer h.opMu.Unlock()
	h.mu.Lock()
	if h.state == Stopped || h.state == Closed {
		h.mu.Unlock()
		return nil
	}
	h.state = Stopping
	active := h.active
	h.mu.Unlock()
	if active != nil {
		active.closeTransport()
	}
	grace := int(s.config.StopGrace.Seconds())
	_, err = s.docker.ContainerStop(ctx, cid, client.ContainerStopOptions{Timeout: &grace})
	if err != nil && !cerrdefs.IsNotFound(err) && !cerrdefs.IsNotModified(err) {
		h.mu.Lock()
		h.state = CleanupFailed
		h.cleanupErr = err
		h.mu.Unlock()
		return err
	}
	h.mu.Lock()
	h.state = Stopped
	h.cleanupErr = nil
	h.mu.Unlock()
	return nil
}

// Close removes only the named, registered sandbox. Failed cleanup retains
// its handle, making the failure visible and another Close safe to retry.
func (s *Supervisor) Close(ctx context.Context, cid string) error {
	h, err := s.get(cid)
	if err != nil {
		return err
	}
	h.opMu.Lock()
	defer h.opMu.Unlock()
	h.mu.Lock()
	if h.state == Closed {
		h.mu.Unlock()
		return nil
	}
	h.state = Closing
	h.cleanupErr = nil
	active := h.active
	h.mu.Unlock()
	if active != nil {
		active.closeTransport()
	}
	grace := int(s.config.StopGrace.Seconds())
	_, stopErr := s.docker.ContainerStop(ctx, cid, client.ContainerStopOptions{Timeout: &grace})
	if cerrdefs.IsNotFound(stopErr) || cerrdefs.IsNotModified(stopErr) {
		stopErr = nil
	}
	if stopErr != nil {
		return s.cleanupFailure(h, fmt.Errorf("stop container: %w", stopErr))
	}
	_, removeErr := s.docker.ContainerRemove(ctx, cid, client.ContainerRemoveOptions{RemoveVolumes: true})
	if cerrdefs.IsNotFound(removeErr) {
		removeErr = nil
	}
	if removeErr != nil {
		return s.cleanupFailure(h, fmt.Errorf("remove container: %w", removeErr))
	}
	h.mu.Lock()
	h.state = Closed
	h.cleanupErr = nil
	h.mu.Unlock()
	s.mu.Lock()
	if s.handles[cid] == h {
		delete(s.handles, cid)
	}
	s.mu.Unlock()
	return nil
}

func (s *Supervisor) cleanupFailure(h *SandboxHandle, err error) error {
	h.mu.Lock()
	h.state = CleanupFailed
	h.cleanupErr = err
	h.mu.Unlock()
	return err
}

// Shutdown rejects new work and closes every registered sandbox. A failed
// cleanup leaves the owner lock held, so the same instance can retry.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	s.mu.Lock()
	s.shuttingDown = true
	s.ready = false
	s.mu.Unlock()
	startsDone := make(chan struct{})
	go func() { s.starts.Wait(); close(startsDone) }()
	select {
	case <-startsDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	var errs []error
	for _, info := range s.List() {
		if err := s.Close(ctx, info.CID); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", info.CID, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.handles) != 0 {
		return errors.New("sandbox registry is not empty")
	}
	if s.lock != nil {
		if err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN); err != nil {
			return err
		}
		if err := s.lock.Close(); err != nil {
			return err
		}
		s.lock = nil
	}
	if s.docker != nil {
		return s.docker.Close()
	}
	return nil
}
