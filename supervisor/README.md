# Go supervisor

`agent-sandbox/supervisor` manages containers created from this repo's image.
It is an embeddable package; the existing `sandbox` script remains available.
Build the image with `./sandbox build` before using the package.

```go
s, err := supervisor.New(supervisor.Config{
    OwnerID:   "my-service", // stable across process restarts
    StateDir:  "/var/lib/my-service/supervisor",
    DockerHost: "unix:///var/run/docker.sock",
})
if err != nil { return err }
if err := s.Initialize(ctx); err != nil { return err }
defer s.Shutdown(context.Background())

h, err := s.Start(ctx, supervisor.SandboxSpec{
    Image:     "agent-sandbox:latest",
    Workspace: "/srv/workspaces/project-123",
    Env:       []string{"ANTHROPIC_API_KEY=" + key},
    CPUs:      2,
    MemoryBytes: 4 << 30,
})
if err != nil { return err }
defer s.Close(context.Background(), h.CID)

cmd, err := s.Exec(ctx, h.CID, supervisor.ExecSpec{
    Args: []string{"python3", "script.py"},
})
if err != nil { return err }
// Drain cmd.Stdout and cmd.Stderr concurrently, then call cmd.Wait(ctx).
// Close cmd.Stdin to send EOF when there is no input.
```

`Initialize` acquires a local lock and removes containers from a prior process
with the same owner label. If removal fails, it retains those handles and
returns an error; retry `Initialize` after Docker is available. `Start` creates
one handle keyed by its Docker container ID. `Stop` retains the handle in the
`stopped` state. `Close` stops and removes the container, then removes the
handle from the registry. Failed cleanup leaves it visible as `cleanup_failed`
for another `Close` attempt. `Shutdown` closes all handles and releases the
owner lock after cleanup succeeds.

Only one `Exec` may run per sandbox. A normal command exit leaves the container
running, including a nonzero exit code. Cancelling the context passed to `Exec`
or losing its Docker stream closes the whole sandbox. `Wait` returns that error;
its own context controls how long the caller waits and does not cancel the
command. Docker streams apply backpressure: drain both outputs concurrently
and close stdin to send EOF. `Exec` passes an argument vector directly, without
shell parsing. The package does not implement an interactive terminal.

The Docker endpoint comes from `DockerHost`, then `DOCKER_HOST`, then Docker's
default local Unix socket. Docker CLI contexts are not resolved automatically.
For OrbStack, set `DockerHost` to the socket shown by
`docker context inspect orbstack --format '{{.Endpoints.docker.Host}}'`.
Do not expose the Docker socket or supervisor methods to untrusted callers.

For EC2, run the supervisor on the host with a persistent `StateDir`, provision
Docker and the correct image architecture, and give the workspace to UID 1000.
Set CPU and memory limits for each sandbox. Do not mount the Docker socket or
AWS credentials into a sandbox. Block container access to EC2 instance metadata
at the host network layer, or disable metadata if the host does not need it.
The package currently targets trusted internal workloads on one host.

Tests: `go test -race ./...` and `go vet ./...`. To include the real Docker
lifecycle test after building the image, set `SUPERVISOR_INTEGRATION=1` and
`DOCKER_HOST` if necessary, then run `go test -run TestDockerLifecycle ./supervisor`.
