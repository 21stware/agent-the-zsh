package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// SocketPath returns the unix socket path for flowd. Order of preference:
//  1. $FLOW_SOCKET (explicit override)
//  2. $XDG_RUNTIME_DIR/flow/flowd.sock
//  3. $TMPDIR/flow-<uid>/flowd.sock (macOS has no XDG_RUNTIME_DIR by default)
//
// The directory is created with 0700 so other users cannot connect.
func SocketPath() (string, error) {
	if p := os.Getenv("FLOW_SOCKET"); p != "" {
		return p, nil
	}
	var base string
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		base = filepath.Join(rt, "flow")
	} else {
		tmp := os.Getenv("TMPDIR")
		if tmp == "" {
			tmp = "/tmp"
		}
		base = filepath.Join(tmp, fmt.Sprintf("flow-%d", os.Getuid()))
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(base, "flowd.sock"), nil
}

// Listen binds a unix socket at path, clearing a stale socket left by a crashed
// daemon. It returns the listener; the caller is responsible for closing it
// (which removes the socket file).
func Listen(path string) (net.Listener, error) {
	// If something is already listening, refuse — don't clobber a live daemon.
	if conn, err := net.Dial("unix", path); err == nil {
		conn.Close()
		return nil, fmt.Errorf("flowd already running at %s", path)
	}
	// Stale socket file from a previous crash: remove it.
	if _, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Socket readable/writable by owner only.
	_ = os.Chmod(path, 0o600)
	return ln, nil
}
