// Package jupyter implements a Jupyter kernel host: it spawns kernel
// processes (`python -m ipykernel_launcher` and friends), talks the
// Jupyter wire protocol (ZMTP / ZMQ) over five sockets, and exposes a
// channel-streaming Execute API to higher layers.
//
// The package is intentionally Go-native: it uses go-zeromq/zmq4 (no
// CGO) so the runtime image stays libzmq-free and the binary stays
// statically linkable. Hot-path allocations are pooled via sync.Pool
// where it matters — buffer reuse on the iopub fan-out, HMAC scratch
// space, JSON encoding buffers.
package jupyter

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// ConnectionFile is the JSON document a Jupyter kernel reads to learn
// which ZMQ ports to bind and which key to sign messages with. We write
// it to a temp file and pass --connection-file=<path> to the launcher.
//
// Field names match the upstream contract exactly; any rename breaks
// kernel compatibility.
type ConnectionFile struct {
	ShellPort       int    `json:"shell_port"`
	IOPubPort       int    `json:"iopub_port"`
	StdinPort       int    `json:"stdin_port"`
	ControlPort     int    `json:"control_port"`
	HBPort          int    `json:"hb_port"`
	IP              string `json:"ip"`
	Key             string `json:"key"`
	Transport       string `json:"transport"`
	SignatureScheme string `json:"signature_scheme"`
	KernelName      string `json:"kernel_name"`
}

// NewConnectionFile allocates five free TCP ports on 127.0.0.1, picks a
// 256-bit signing key, and returns a populated ConnectionFile. The
// ports are reserved by an immediate Listen+Close, so there's a tiny
// race window before the kernel rebinds them — acceptable in practice
// because the kernel boots within ~50ms.
func NewConnectionFile(kernelName string) (*ConnectionFile, error) {
	ports, err := allocPorts(5)
	if err != nil {
		return nil, err
	}
	key, err := randomKey(32)
	if err != nil {
		return nil, err
	}
	return &ConnectionFile{
		ShellPort:       ports[0],
		IOPubPort:       ports[1],
		StdinPort:       ports[2],
		ControlPort:     ports[3],
		HBPort:          ports[4],
		IP:              "127.0.0.1",
		Key:             key,
		Transport:       "tcp",
		SignatureScheme: "hmac-sha256",
		KernelName:      kernelName,
	}, nil
}

// WriteTo serializes the file to a fresh temp file under dir (or the OS
// default tmp if dir is empty) and returns its absolute path. Callers
// own cleanup.
func (c *ConnectionFile) WriteTo(dir string) (string, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, "ad-sandbox-kernel-*.json")
	if err != nil {
		return "", fmt.Errorf("create connection file: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("encode connection file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	abs, err := filepath.Abs(f.Name())
	if err != nil {
		return f.Name(), nil
	}
	return abs, nil
}

// Endpoint formats a "tcp://ip:port" address for a named socket. Useful
// when wiring up the ZMQ Dial calls.
func (c *ConnectionFile) Endpoint(socket string) string {
	port := 0
	switch socket {
	case "shell":
		port = c.ShellPort
	case "iopub":
		port = c.IOPubPort
	case "stdin":
		port = c.StdinPort
	case "control":
		port = c.ControlPort
	case "hb":
		port = c.HBPort
	}
	return fmt.Sprintf("%s://%s:%d", c.Transport, c.IP, port)
}

// allocPorts asks the kernel for n free TCP ports by listening on :0
// and immediately closing. The ports are very likely still free when
// the Jupyter kernel rebinds them a few ms later.
func allocPorts(n int) ([]int, error) {
	out := make([]int, 0, n)
	listeners := make([]*net.TCPListener, 0, n)

	// Keep listeners open until we've grabbed all n so the kernel
	// doesn't immediately collide on the same port.
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()

	for i := 0; i < n; i++ {
		l, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return nil, fmt.Errorf("alloc port: %w", err)
		}
		listeners = append(listeners, l)
		out = append(out, l.Addr().(*net.TCPAddr).Port)
	}
	return out, nil
}

// randomKey returns a hex string of n random bytes. Used both for the
// HMAC signing key and for message ids.
func randomKey(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
