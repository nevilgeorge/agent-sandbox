package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const ownerLabel = "agent-sandbox.supervisor.owner"
const instanceLabel = "agent-sandbox.supervisor.instance"
const readyFile = "/tmp/.agent-sandbox-ready"

type Supervisor struct {
	config       Config
	docker       dockerAPI
	initMu       sync.Mutex
	mu           sync.RWMutex
	starts       sync.WaitGroup
	handles      map[string]*SandboxHandle
	ready        bool
	shuttingDown bool
	lock         *os.File
}

func New(config Config) (*Supervisor, error) {
	if strings.TrimSpace(config.OwnerID) == "" || strings.ContainsAny(config.OwnerID, "\x00\n\r") {
		return nil, errors.New("OwnerID must be a nonempty single-line value")
	}
	if config.StateDir == "" {
		return nil, errors.New("StateDir is required")
	}
	if config.StartupTimeout <= 0 {
		config.StartupTimeout = 30 * time.Second
	}
	if config.StopGrace <= 0 {
		config.StopGrace = 10 * time.Second
	}
	if config.CleanupTimeout <= 0 {
		config.CleanupTimeout = 30 * time.Second
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if config.DockerHost != "" {
		opts = append(opts, client.WithHost(config.DockerHost))
	} else {
		opts = append(opts, client.WithHostFromEnv())
	}
	d, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, err
	}
	return &Supervisor{config: config, docker: d, handles: make(map[string]*SandboxHandle)}, nil
}

// Initialize locks this owner's state directory and removes its abandoned
// containers. A failed cleanup leaves its handle registered for retry.
func (s *Supervisor) Initialize(ctx context.Context) error {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return nil
	}
	if s.shuttingDown {
		return ErrUnavailable
	}
	if s.lock == nil {
		if err := os.MkdirAll(s.config.StateDir, 0700); err != nil {
			return err
		}
		path := filepath.Join(s.config.StateDir, "supervisor.lock")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			return fmt.Errorf("owner lock: %w", err)
		}
		s.lock = f
	}
	if _, err := s.docker.Ping(ctx, client.PingOptions{}); err != nil {
		return fmt.Errorf("Docker ping: %w", err)
	}
	filters := make(client.Filters).Add("label", ownerLabel+"="+s.config.OwnerID)
	listed, err := s.docker.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return fmt.Errorf("list owned containers: %w", err)
	}
	for _, c := range listed.Items {
		if c.Labels[ownerLabel] != s.config.OwnerID {
			continue
		}
		if _, ok := s.handles[c.ID]; !ok {
			s.handles[c.ID] = &SandboxHandle{CID: c.ID, state: Stopped}
		}
	}
	// Close outside the registry lock. Existing failed handles are retried too.
	ids := make([]string, 0, len(s.handles))
	for id := range s.handles {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if err := s.Close(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	s.mu.Lock()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	s.ready = true
	return nil
}

func (s *Supervisor) Lookup(cid string) (SandboxInfo, bool) {
	s.mu.RLock()
	h, ok := s.handles[cid]
	s.mu.RUnlock()
	if !ok {
		return SandboxInfo{}, false
	}
	return snapshot(h), true
}

func (s *Supervisor) List() []SandboxInfo {
	s.mu.RLock()
	hs := make([]*SandboxHandle, 0, len(s.handles))
	for _, h := range s.handles {
		hs = append(hs, h)
	}
	s.mu.RUnlock()
	out := make([]SandboxInfo, 0, len(hs))
	for _, h := range hs {
		out = append(out, snapshot(h))
	}
	return out
}

func snapshot(h *SandboxHandle) SandboxInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := SandboxInfo{CID: h.CID, State: h.state}
	if h.cleanupErr != nil {
		i.CleanupError = h.cleanupErr.Error()
	}
	return i
}

func (s *Supervisor) get(cid string) (*SandboxHandle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.handles[cid]
	if h == nil {
		return nil, ErrNotFound
	}
	return h, nil
}

func validEnv(env []string) error {
	for _, item := range env {
		key, _, ok := strings.Cut(item, "=")
		if !ok || key == "" || strings.ContainsAny(key, "\x00\n\r") || strings.ContainsRune(item, 0) {
			return fmt.Errorf("invalid environment entry %q", key)
		}
	}
	return nil
}

func bind(source, target string, ro bool) (mount.Mount, error) {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target || target == "/" {
		return mount.Mount{}, fmt.Errorf("invalid mount target %q", target)
	}
	path, err := filepath.Abs(source)
	if err != nil {
		return mount.Mount{}, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return mount.Mount{}, err
	}
	if _, err = os.Stat(path); err != nil {
		return mount.Mount{}, err
	}
	return mount.Mount{Type: mount.TypeBind, Source: path, Target: target, ReadOnly: ro}, nil
}

func (s *Supervisor) Start(ctx context.Context, spec SandboxSpec) (*SandboxHandle, error) {
	s.mu.Lock()
	ready := s.ready && !s.shuttingDown
	if ready {
		s.starts.Add(1)
	}
	s.mu.Unlock()
	if !ready {
		return nil, ErrNotReady
	}
	defer s.starts.Done()
	if spec.Image == "" {
		return nil, errors.New("image is required")
	}
	if err := validEnv(spec.Env); err != nil {
		return nil, err
	}
	if spec.CPUs < 0 || math.IsNaN(spec.CPUs) || math.IsInf(spec.CPUs, 0) || spec.MemoryBytes < 0 || spec.PidsLimit < 0 {
		return nil, errors.New("invalid resource limit")
	}
	work, err := bind(spec.Workspace, "/workspace", false)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	st, err := os.Stat(work.Source)
	if err != nil || !st.IsDir() {
		return nil, errors.New("workspace must be a directory")
	}
	mounts := []mount.Mount{work}
	seen := map[string]bool{"/workspace": true}
	for _, m := range spec.Mounts {
		b, err := bind(m.Source, m.Target, m.ReadOnly)
		if err != nil {
			return nil, err
		}
		if seen[b.Target] || strings.HasPrefix(b.Target, "/workspace/") {
			return nil, fmt.Errorf("overlapping mount target %q", b.Target)
		}
		seen[b.Target] = true
		mounts = append(mounts, b)
	}
	if _, err := s.docker.ImageInspect(ctx, spec.Image); err != nil {
		return nil, fmt.Errorf("inspect image: %w", err)
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	instance := hex.EncodeToString(random[:])
	name := "agent-sandbox-" + instance
	pids := spec.PidsLimit
	if pids == 0 {
		pids = 2048
	}
	init := true
	host := &container.HostConfig{
		Init: &init, AutoRemove: false, SecurityOpt: []string{"no-new-privileges"},
		Mounts: mounts, Resources: container.Resources{PidsLimit: &pids, Memory: spec.MemoryBytes, NanoCPUs: int64(spec.CPUs * 1e9)},
		LogConfig: container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
	}
	conf := &container.Config{Image: spec.Image, WorkingDir: "/workspace", Env: append([]string(nil), spec.Env...),
		Cmd:    []string{"bash", "-c", "touch " + readyFile + " && exec sleep infinity"},
		Labels: map[string]string{ownerLabel: s.config.OwnerID, instanceLabel: instance}}
	created, err := s.docker.ContainerCreate(ctx, client.ContainerCreateOptions{Config: conf, HostConfig: host, Name: name})
	if err != nil {
		// The daemon may have created the container before the connection failed.
		probeCtx, cancel := context.WithTimeout(context.Background(), s.config.CleanupTimeout)
		defer cancel()
		found, probeErr := s.docker.ContainerInspect(probeCtx, name, client.ContainerInspectOptions{})
		if probeErr != nil || found.Container.Config == nil || found.Container.Config.Labels[instanceLabel] != instance || found.Container.Config.Labels[ownerLabel] != s.config.OwnerID {
			return nil, fmt.Errorf("create container: %w (reconciliation: %v)", err, probeErr)
		}
		created.ID = found.Container.ID
	}
	h := &SandboxHandle{CID: created.ID, state: Starting}
	h.opMu.Lock()
	s.mu.Lock()
	s.handles[h.CID] = h
	s.mu.Unlock()
	if _, err := s.docker.ContainerStart(ctx, h.CID, client.ContainerStartOptions{}); err != nil {
		h.opMu.Unlock()
		return s.failStart(h, err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, s.config.StartupTimeout)
	defer cancel()
	if err := s.awaitReady(readyCtx, h.CID, hasOpenAIKey(spec.Env)); err != nil {
		h.opMu.Unlock()
		return s.failStart(h, err)
	}
	h.mu.Lock()
	h.state = Running
	h.mu.Unlock()
	go s.monitor(h)
	h.opMu.Unlock()
	return h, nil
}

func hasOpenAIKey(env []string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, "OPENAI_API_KEY=") && entry != "OPENAI_API_KEY=" {
			return true
		}
	}
	return false
}

func (s *Supervisor) failStart(h *SandboxHandle, cause error) (*SandboxHandle, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.config.CleanupTimeout)
	defer cancel()
	return h, errors.Join(cause, s.Close(ctx, h.CID))
}

func (s *Supervisor) awaitReady(ctx context.Context, cid string, auth bool) error {
	for {
		inspected, err := s.docker.ContainerInspect(ctx, cid, client.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		if inspected.Container.State == nil || !inspected.Container.State.Running {
			return errors.New("container exited before readiness")
		}
		if err := s.probe(ctx, cid, readyFile); err == nil {
			if !auth {
				return nil
			}
			return s.probe(ctx, cid, "/home/agent/.codex/auth.json")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *Supervisor) probe(ctx context.Context, cid, path string) error {
	created, err := s.docker.ExecCreate(ctx, cid, client.ExecCreateOptions{Cmd: []string{"test", "-f", path}})
	if err != nil {
		return err
	}
	if _, err = s.docker.ExecStart(ctx, created.ID, client.ExecStartOptions{Detach: true}); err != nil {
		return err
	}
	for {
		status, err := s.docker.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return err
		}
		if !status.Running {
			if status.ExitCode != 0 {
				return fmt.Errorf("readiness probe exited %d", status.ExitCode)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *Supervisor) monitor(h *SandboxHandle) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.Lock()
		state := h.state
		h.mu.Unlock()
		if state != Running {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		inspected, err := s.docker.ContainerInspect(ctx, h.CID, client.ContainerInspectOptions{})
		cancel()
		if err != nil && !cerrdefs.IsNotFound(err) {
			s.config.Logger.Warn("inspect sandbox", "cid", h.CID, "error", err)
			continue
		}
		if cerrdefs.IsNotFound(err) || inspected.Container.State == nil || !inspected.Container.State.Running {
			cleanup, done := context.WithTimeout(context.Background(), s.config.CleanupTimeout)
			if err := s.Close(cleanup, h.CID); err != nil {
				s.config.Logger.Error("clean up exited sandbox", "cid", h.CID, "error", err)
			}
			done()
			return
		}
	}
}
