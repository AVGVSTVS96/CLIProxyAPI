//go:build unix

package api

import (
	"net"
	"os"
	"strconv"
	"syscall"
)

// socketActivated reports whether systemd passed this process one listening socket (fd 3).
func socketActivated() bool {
	return os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) && os.Getenv("LISTEN_FDS") == "1"
}

// The inherited socket would otherwise leak into every child process started before Start.
func init() {
	if socketActivated() {
		syscall.CloseOnExec(3)
	}
}

// listen takes the socket systemd passes in when started by a socket unit, so a restart
// queues new connections in the kernel instead of refusing them; otherwise it opens addr.
func listen(addr string) (net.Listener, error) {
	if !socketActivated() {
		return net.Listen("tcp", addr)
	}
	_ = os.Unsetenv("LISTEN_PID")
	_ = os.Unsetenv("LISTEN_FDS")
	_ = os.Unsetenv("LISTEN_FDNAMES")
	file := os.NewFile(3, "systemd-socket")
	defer func() { _ = file.Close() }()
	return net.FileListener(file)
}
