// Package sandbox — containerd backend.
//
// This file implements the "containerd" backend for the Executor
// interface using github.com/containerd/containerd/v2. Each Run call
// creates a short-lived container from the configured image, executes
// the command, captures combined stdout+stderr, and tears the
// container down. The backend requires a running containerd daemon
// reachable via the configured socket (default
// /run/containerd/containerd.sock).
//
// When the socket is not accessible, New returns a clear error so
// callers know to either install containerd or fall back to
// sandbox=exec. Tests t.Skip when the socket is missing.
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"

	"github.com/saker-ai/ctxhub/internal/bot/config"
)

// defaultContainerdSocket is the conventional containerd address.
const defaultContainerdSocket = "/run/containerd/containerd.sock"

// containerdNamespace is the containerd namespace the bot runs in.
// "vikingbot" keeps bot containers out of the default k8s/moby namespace.
const containerdNamespace = "vikingbot"

// ContainerdExecutor runs commands inside short-lived containers via
// containerd. It is safe for concurrent use; the containerd client is
// shared across calls.
type ContainerdExecutor struct {
	cfg     config.SandboxConfig
	client  *containerd.Client
	image   string
	timeout time.Duration
	workDir string
}

// NewContainerd constructs a containerd-backed Executor. socket is the
// containerd grpc address (e.g. /run/containerd/containerd.sock); when
// empty, the default is used. image is the OCI image ref used as the
// base for each Run (e.g. "docker.io/busybox:latest"). The image must
// be present in the containerd image store — Run pulls it on first use.
//
// If the containerd socket is not accessible, New returns a clear error
// describing the missing prerequisite.
func NewContainerd(cfg config.SandboxConfig, socket, image string) (*ContainerdExecutor, error) {
	if socket == "" {
		socket = defaultContainerdSocket
	}
	if image == "" {
		image = "docker.io/busybox:latest"
	}
	// Verify the socket is accessible before we attempt to dial. This
	// surfaces a clear, actionable error rather than a gRPC timeout.
	if _, err := os.Stat(socket); err != nil {
		return nil, fmt.Errorf("sandbox: containerd socket %q not accessible; install containerd or use sandbox=exec: %w", socket, err)
	}
	client, err := containerd.New(socket, containerd.WithDefaultNamespace(containerdNamespace))
	if err != nil {
		return nil, fmt.Errorf("sandbox: dial containerd %q: %w", socket, err)
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if cfg.Timeout <= 0 {
		timeout = 30 * time.Second
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = "/tmp/vikingbot-sandbox"
	}
	return &ContainerdExecutor{
		cfg:     cfg,
		client:  client,
		image:   image,
		timeout: timeout,
		workDir: workDir,
	}, nil
}

// Close releases the containerd client. Safe to call multiple times.
func (e *ContainerdExecutor) Close() error {
	if e.client == nil {
		return nil
	}
	return e.client.Close()
}

// Run implements Executor. It creates a container from the configured
// image, starts the task, waits for completion, captures combined
// stdout+stderr, and tears the container down. The run is bounded by
// the configured timeout; ctx cancellation propagates to the
// container's task.
func (e *ContainerdExecutor) Run(ctx context.Context, cmd string, args ...string) ([]byte, error) {
	if cmd == "" {
		return nil, fmt.Errorf("sandbox: empty command")
	}
	if e.client == nil {
		return nil, fmt.Errorf("sandbox: containerd client not initialized")
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	image, err := e.ensureImage(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox: ensure image: %w", err)
	}

	id := fmt.Sprintf("vikingbot-%d", time.Now().UnixNano())
	absWorkDir, err := filepath.Abs(e.workDir)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve workdir: %w", err)
	}

	var stdout, stderr bytes.Buffer
	ioCreator := cio.NewCreator(cio.WithStreams(nil, &stdout, &stderr))

	container, err := e.client.NewContainer(
		ctx,
		id,
		containerd.WithImage(image),
		containerd.WithNewSnapshot(id, image),
		containerd.WithNewSpec(
			oci.WithImageConfig(image),
			oci.WithProcessArgs(append([]string{cmd}, args...)...),
			oci.WithProcessCwd(absWorkDir),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("sandbox: create container: %w", err)
	}
	defer func() {
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
	}()

	task, err := container.NewTask(ctx, ioCreator)
	if err != nil {
		return nil, fmt.Errorf("sandbox: create task: %w", err)
	}
	defer func() {
		_, _ = task.Delete(ctx)
	}()

	if err := task.Start(ctx); err != nil {
		return nil, fmt.Errorf("sandbox: start task: %w", err)
	}

	exitCh, err := task.Wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox: wait task: %w", err)
	}
	select {
	case status := <-exitCh:
		out := append(stdout.Bytes(), stderr.Bytes()...)
		if status.ExitCode() != 0 {
			return out, fmt.Errorf("sandbox: run %s: exit %d", cmd, status.ExitCode())
		}
		return out, nil
	case <-ctx.Done():
		// Best-effort kill on timeout/cancel.
		_ = task.Kill(ctx, 9)
		return append(stdout.Bytes(), stderr.Bytes()...), fmt.Errorf("sandbox: run %s: %w", cmd, ctx.Err())
	}
}

// ensureImage returns the image from the containerd image store,
// pulling it on first use. The pull unpacks the image into the
// overlayfs snapshotter for the host platform so the subsequent
// NewContainer / NewTask call can mount the rootfs.
func (e *ContainerdExecutor) ensureImage(ctx context.Context) (containerd.Image, error) {
	img, err := e.client.GetImage(ctx, e.image)
	if err == nil {
		// Even when the image is already in the store, the snapshotter
		// may not have the unpacked layers. We try Unpack first; it's
		// a no-op when the snapshot already exists.
		if err := img.Unpack(ctx, "overlayfs"); err != nil {
			return nil, fmt.Errorf("unpack %s: %w", e.image, err)
		}
		return img, nil
	}
	// Pull on first use. WithPlatform selects the host's platform
	// manifest (the image may be a multi-arch index); WithPullSnapshotter
	// triggers unpack into the overlayfs snapshotter so NewContainer
	// can mount the rootfs.
	img, err = e.client.Pull(ctx, e.image,
		containerd.WithPullSnapshotter("overlayfs"),
		containerd.WithPlatform("linux/"+runtime.GOARCH),
	)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", e.image, err)
	}
	return img, nil
}

// Compile-time assertion that ContainerdExecutor satisfies Executor.
var _ Executor = (*ContainerdExecutor)(nil)
