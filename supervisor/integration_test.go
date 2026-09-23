package supervisor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// Set SUPERVISOR_INTEGRATION=1 to test against a local Docker daemon and the
// image built by ./sandbox build.
func TestDockerLifecycle(t *testing.T) {
	if os.Getenv("SUPERVISOR_INTEGRATION") != "1" {
		t.Skip("requires a local Docker daemon and agent-sandbox:latest")
	}
	s, err := New(Config{OwnerID: "go-integration-test", StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := s.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	h, err := s.Start(ctx, SandboxSpec{Image: "agent-sandbox:latest", Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("start (%v): %v", h, err)
	}
	e, err := s.Exec(ctx, h.CID, ExecSpec{Args: []string{"bash", "-c", "cat; printf 'problem\\n' >&2; exit 7"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Stdin.Write([]byte("hello\x00world\n")); err != nil {
		t.Fatal(err)
	}
	if err := e.Stdin.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr []byte
	var outErr, errErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); stdout, outErr = io.ReadAll(e.Stdout) }()
	go func() { defer wg.Done(); stderr, errErr = io.ReadAll(e.Stderr) }()
	wg.Wait()
	result, err := e.Wait(ctx)
	if err != nil || outErr != nil || errErr != nil {
		t.Fatalf("exec: %v; stdout: %v; stderr: %v", err, outErr, errErr)
	}
	if result.ExitCode != 7 || !bytes.Equal(stdout, []byte("hello\x00world\n")) || string(stderr) != "problem\n" {
		t.Fatalf("unexpected result: exit %d stdout %q stderr %q", result.ExitCode, stdout, stderr)
	}
	if info, ok := s.Lookup(h.CID); !ok || info.State != Running {
		t.Fatalf("sandbox did not survive normal exec: %+v %v", info, ok)
	}
	commandCtx, cancelCommand := context.WithCancel(ctx)
	long, err := s.Exec(commandCtx, h.CID, ExecSpec{Args: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	cancelCommand()
	_, err = long.Wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled exec: %v", err)
	}
	if _, ok := s.Lookup(h.CID); ok {
		t.Fatal("cancelled sandbox remains registered")
	}
	stopped, err := s.Start(ctx, SandboxSpec{Image: "agent-sandbox:latest", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := s.Exec(ctx, stopped.CID, ExecSpec{Args: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec(ctx, stopped.CID, ExecSpec{Args: []string{"true"}}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second exec: %v", err)
	}
	if err := s.Stop(ctx, stopped.CID); err != nil {
		t.Fatal(err)
	}
	_, err = blocked.Wait(ctx)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stopped exec: %v", err)
	}
	if info, ok := s.Lookup(stopped.CID); !ok || info.State != Stopped {
		t.Fatalf("stop did not retain handle: %+v %v", info, ok)
	}
	if err := s.Close(ctx, stopped.CID); err != nil {
		t.Fatal(err)
	}
}
