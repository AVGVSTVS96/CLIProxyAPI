//go:build unix

// Package handoff lets a new proxy process take over the listening socket while the old
// one finishes its in-flight requests, so a binary swap never makes clients wait on the
// slowest answer. `systemctl reload` (SIGHUP) starts it; `restart` still stops first.
package handoff

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// env marks a successor: fd 3 is the listening socket and fd 4 talks to the old process.
const env = "CLIPROXY_HANDOFF"

const (
	listenerFD = 3
	controlFD  = 4
)

var inherited = os.Getenv(env) == "1"

func init() {
	if inherited {
		_ = os.Unsetenv(env)
		syscall.CloseOnExec(listenerFD)
		syscall.CloseOnExec(controlFD)
	}
}

// Requests delivers a signal each time the service manager asks for a reload. Without
// systemd's notify socket there is nobody to tell about the new process, so SIGHUP keeps
// its default meaning.
func Requests() <-chan os.Signal {
	if os.Getenv("NOTIFY_SOCKET") == "" {
		return nil
	}
	requests := make(chan os.Signal, 1)
	signal.Notify(requests, syscall.SIGHUP)
	return requests
}

// Inherited reports whether this process was started to take over from a running one.
func Inherited() bool { return inherited }

var (
	opened    = make(chan struct{})
	openOnce  sync.Once
	successor struct {
		control *os.File
		reader  *bufio.Reader
	}
)

// Listener returns the old process's socket; it accepts nothing until Complete.
func Listener() (net.Listener, error) {
	file := os.NewFile(listenerFD, "handoff-socket")
	defer func() { _ = file.Close() }()
	listener, errListen := net.FileListener(file)
	if errListen != nil {
		return nil, errListen
	}
	successor.control = os.NewFile(controlFD, "handoff-control")
	successor.reader = bufio.NewReader(successor.control)
	return &gatedListener{Listener: listener, closed: make(chan struct{})}, nil
}

// Complete tells the old process this one is ready, waits for it to stop accepting, runs
// takeover, starts accepting and makes this process the service's main one.
func Complete(takeover func()) error {
	control := successor.control
	if control == nil {
		return errors.New("no old process to take over from")
	}
	defer func() { _ = control.Close() }()
	if _, errWrite := io.WriteString(control, "ready\n"); errWrite != nil {
		return errWrite
	}
	if errExpect := expect(successor.reader, "go"); errExpect != nil {
		return errExpect
	}
	takeover()
	openOnce.Do(func() { close(opened) })
	if errNotify := notify(fmt.Sprintf("MAINPID=%d\nREADY=1", os.Getpid())); errNotify != nil {
		return fmt.Errorf("tell systemd: %w", errNotify)
	}
	_, errWrite := io.WriteString(control, "done\n")
	return errWrite
}

type gatedListener struct {
	net.Listener
	closeOnce sync.Once
	closed    chan struct{}
}

func (l *gatedListener) Accept() (net.Conn, error) {
	select {
	case <-opened:
		return l.Listener.Accept()
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// File lets this process hand the socket on at its own next reload.
func (l *gatedListener) File() (*os.File, error) {
	filer, ok := l.Listener.(interface{ File() (*os.File, error) })
	if !ok {
		return nil, fmt.Errorf("listener %T cannot be handed over", l.Listener)
	}
	return filer.File()
}

func (l *gatedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// Successor is a new process starting to take over this one's socket.
type Successor struct {
	cmd     *exec.Cmd
	control *os.File
	reader  *bufio.Reader
	exited  chan error
}

// Begin starts this binary again, from its path on disk so a replaced binary is the one
// that runs, with the same arguments and the listening socket.
func Begin(listener *os.File) (*Successor, error) {
	pair, errPair := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if errPair != nil {
		return nil, errPair
	}
	mine, theirs := os.NewFile(uintptr(pair[0]), "handoff-control"), os.NewFile(uintptr(pair[1]), "handoff-control")
	defer func() { _ = theirs.Close() }()
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = append(os.Environ(), env+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{listener, theirs}
	if errStart := cmd.Start(); errStart != nil {
		_ = mine.Close()
		return nil, errStart
	}
	s := &Successor{cmd: cmd, control: mine, reader: bufio.NewReader(mine), exited: make(chan error, 1)}
	go func() { s.exited <- cmd.Wait() }()
	return s, nil
}

// Ready waits until the successor is fully started, or fails if it exits or takes too long.
func (s *Successor) Ready(limit time.Duration) error {
	ready := make(chan error, 1)
	go func() { ready <- expect(s.reader, "ready") }()
	select {
	case errReady := <-ready:
		return errReady
	case errExit := <-s.exited:
		return fmt.Errorf("new process exited while starting: %v", errExit)
	case <-time.After(limit):
		return fmt.Errorf("new process not ready after %s", limit)
	}
}

// Commit hands over to the successor and returns once systemd tracks it as the main process.
func (s *Successor) Commit() error {
	defer func() { _ = s.control.Close() }()
	if _, errWrite := io.WriteString(s.control, "go\n"); errWrite != nil {
		return errWrite
	}
	return expect(s.reader, "done")
}

// Abort stops a successor that will not take over.
func (s *Successor) Abort() {
	_ = s.control.Close()
	_ = s.cmd.Process.Kill()
}

func expect(reader *bufio.Reader, word string) error {
	line, errRead := reader.ReadString('\n')
	if errRead != nil {
		return fmt.Errorf("waiting for %q: %w", word, errRead)
	}
	if got := strings.TrimSpace(line); got != word {
		return fmt.Errorf("got %q, want %q", got, word)
	}
	return nil
}

// notify sends state to systemd and waits until systemd has processed it, so the old
// process never exits while systemd still counts it as the main one.
func notify(state string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	fd, errSocket := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if errSocket != nil {
		return errSocket
	}
	defer func() { _ = syscall.Close(fd) }()
	to := &syscall.SockaddrUnix{Name: socket}
	if errSend := syscall.Sendmsg(fd, []byte(state), nil, to, 0); errSend != nil {
		return errSend
	}
	reader, writer, errPipe := os.Pipe()
	if errPipe != nil {
		return errPipe
	}
	defer func() { _ = reader.Close() }()
	errBarrier := syscall.Sendmsg(fd, []byte("BARRIER=1"), syscall.UnixRights(int(writer.Fd())), to, 0)
	_ = writer.Close()
	if errBarrier != nil {
		return errBarrier
	}
	_ = reader.SetReadDeadline(time.Now().Add(30 * time.Second))
	if _, errRead := reader.Read(make([]byte, 1)); !errors.Is(errRead, io.EOF) {
		return fmt.Errorf("systemd barrier: %v", errRead)
	}
	return nil
}
