package supervisor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type fakeDocker struct {
	dockerAPI
	mu          sync.Mutex
	stopCalls   int
	removeCalls int
	removeErr   error
	containers  []container.Summary
}

func (f *fakeDocker) Ping(context.Context, client.PingOptions) (client.PingResult, error) {
	return client.PingResult{}, nil
}
func (f *fakeDocker) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: f.containers}, nil
}
func (f *fakeDocker) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	f.stopCalls++
	f.mu.Unlock()
	return client.ContainerStopResult{}, cerrdefs.ErrNotModified
}
func (f *fakeDocker) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls++
	return client.ContainerRemoveResult{}, f.removeErr
}
func (f *fakeDocker) Close() error { return nil }

func newFakeSupervisor(f *fakeDocker) *Supervisor {
	return &Supervisor{config: Config{OwnerID: "test-owner", StopGrace: time.Second, CleanupTimeout: time.Second}, docker: f, handles: make(map[string]*SandboxHandle)}
}

func TestCloseKeepsFailedCleanupRegistered(t *testing.T) {
	f := &fakeDocker{removeErr: errors.New("daemon disconnected")}
	s := newFakeSupervisor(f)
	h := &SandboxHandle{CID: "cid-1", state: Running}
	s.handles[h.CID] = h
	if err := s.Close(context.Background(), h.CID); err == nil {
		t.Fatal("expected remove error")
	}
	info, ok := s.Lookup(h.CID)
	if !ok || info.State != CleanupFailed || info.CleanupError == "" {
		t.Fatalf("failed cleanup lost from registry: %+v %v", info, ok)
	}
	f.removeErr = nil
	if err := s.Close(context.Background(), h.CID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(h.CID); ok {
		t.Fatal("removed sandbox is still registered")
	}
	if f.removeCalls != 2 {
		t.Fatalf("remove calls: %d", f.removeCalls)
	}
}

func TestConcurrentCloseRemovesOnce(t *testing.T) {
	f := &fakeDocker{}
	s := newFakeSupervisor(f)
	s.handles["cid-2"] = &SandboxHandle{CID: "cid-2", state: Running}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Close(context.Background(), "cid-2")
			if err != nil && !errors.Is(err, ErrNotFound) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.removeCalls != 1 {
		t.Fatalf("removed %d times", f.removeCalls)
	}
}

func TestInitializeOnlyCleansOwnedContainers(t *testing.T) {
	f := &fakeDocker{containers: []container.Summary{
		{ID: "ours", Labels: map[string]string{ownerLabel: "test-owner"}},
		{ID: "other", Labels: map[string]string{ownerLabel: "someone-else"}},
		{ID: "bash", Labels: map[string]string{"agent-sandbox": "1"}},
	}}
	s := newFakeSupervisor(f)
	s.config.StateDir = t.TempDir()
	if err := s.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.removeCalls != 1 {
		t.Fatalf("removed %d containers, want only one", f.removeCalls)
	}
	if len(s.List()) != 0 {
		t.Fatal("recovery left an owned handle")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerLock(t *testing.T) {
	dir := t.TempDir()
	a := newFakeSupervisor(&fakeDocker{})
	a.config.StateDir = dir
	b := newFakeSupervisor(&fakeDocker{})
	b.config.StateDir = dir
	if err := a.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Initialize(context.Background()); err == nil {
		t.Fatal("second supervisor acquired same owner lock")
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStopRetainsHandle(t *testing.T) {
	f := &fakeDocker{}
	s := newFakeSupervisor(f)
	s.handles["cid-3"] = &SandboxHandle{CID: "cid-3", state: Running}
	if err := s.Stop(context.Background(), "cid-3"); err != nil {
		t.Fatal(err)
	}
	info, ok := s.Lookup("cid-3")
	if !ok || info.State != Stopped {
		t.Fatalf("stop lost handle: %+v %v", info, ok)
	}
	if err := s.Close(context.Background(), "cid-3"); err != nil {
		t.Fatal(err)
	}
}
