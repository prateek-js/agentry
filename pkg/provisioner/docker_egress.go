package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"
)

// applyEgress installs nft rules in the main container's network
// namespace by spawning a transient sidecar that joins it. The sidecar
// has CAP_NET_ADMIN; the main container does not. Once the sidecar
// exits the rules persist in the netns because the netns is pinned by
// the main container's init pid.
//
// Why a sidecar and not `docker exec`: exec inherits caps from the
// target container, so to use exec we'd have to grant the main
// container CAP_NET_ADMIN — which would let the LLM inside flush our
// rules with `nft flush ruleset`. The sidecar pattern is the only way
// to apply rules the workload can't reverse.
//
// Why nft and not iptables-restore: nft is the modern interface and
// the rule language we render in egress.go is nft syntax.
func (d *DockerBackend) applyEgress(ctx context.Context, mainName, img string, policy EgressPolicy) error {
	script := policy.RenderNFT()
	if script == "" {
		return nil
	}

	// The sidecar runs in the same image so we know nft is present
	// (the image bakes it). `sh -c` lets us pipe the rendered script
	// into `nft -f -` without writing it to disk first.
	cmd := []string{"sh", "-c", "nft -f -"}

	hostCfg := &container.HostConfig{
		// Join the main container's netns. This is what makes the nft
		// rules apply to *its* traffic, not the sidecar's host-side.
		NetworkMode: container.NetworkMode("container:" + mainName),
		AutoRemove:  true,
		CapAdd:      []string{"NET_ADMIN"},
	}
	contCfg := &container.Config{
		Image:        img,
		Cmd:          cmd,
		OpenStdin:    true,
		StdinOnce:    true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
		Labels: map[string]string{
			"app":               "ad-sandbox-egress-init",
			"sandbox-id-target": mainName,
		},
	}

	resp, err := d.cli.ContainerCreate(ctx, contCfg, hostCfg, &network.NetworkingConfig{}, nil, "")
	if err != nil {
		return fmt.Errorf("egress sidecar create: %w", err)
	}
	cid := resp.ID

	// Attach stdin BEFORE start — Docker's attach must be established
	// before the process reads from stdin, or we'll deadlock waiting
	// for input that never arrives.
	hijack, err := d.cli.ContainerAttach(ctx, cid, container.AttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		_ = d.cli.ContainerRemove(ctx, cid, container.RemoveOptions{Force: true})
		return fmt.Errorf("egress sidecar attach: %w", err)
	}
	defer hijack.Close()

	if err := d.cli.ContainerStart(ctx, cid, container.StartOptions{}); err != nil {
		return fmt.Errorf("egress sidecar start: %w", err)
	}

	// Pipe the nft script in, close stdin so `nft -f -` sees EOF.
	if _, err := io.WriteString(hijack.Conn, script); err != nil {
		return fmt.Errorf("egress sidecar write: %w", err)
	}
	if err := hijack.CloseWrite(); err != nil {
		return fmt.Errorf("egress sidecar close stdin: %w", err)
	}

	// Drain output so we can surface nft errors (parse errors, kernel
	// rejecting a rule, …) in the API response.
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, hijack.Reader); err != nil && err != io.EOF {
		return fmt.Errorf("egress sidecar drain: %w", err)
	}

	// Wait for the sidecar to exit so the netns has the rules installed
	// before we return control to the caller. AutoRemove takes care of
	// the cleanup either way.
	statusCh, errCh := d.cli.ContainerWait(ctx, cid, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return fmt.Errorf("egress sidecar wait: %w", err)
	case status := <-statusCh:
		if status.StatusCode != 0 {
			return fmt.Errorf("egress sidecar exit=%d: %s", status.StatusCode, stderr.String())
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
