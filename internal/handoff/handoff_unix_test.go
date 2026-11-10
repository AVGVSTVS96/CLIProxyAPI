//go:build unix

package handoff

import (
	"bufio"
	"net"
	"os"
	"testing"
	"time"
)

// TestMain runs the test binary as the successor when Begin starts it again.
func TestMain(m *testing.M) {
	if Inherited() {
		os.Exit(runSuccessor())
	}
	os.Exit(m.Run())
}

func runSuccessor() int {
	listener, errListen := Listener()
	if errListen != nil {
		return 2
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		if conn, errAccept := listener.Accept(); errAccept == nil {
			_, _ = conn.Write([]byte("successor\n"))
			_ = conn.Close()
		}
	}()
	if errComplete := Complete(func() {}); errComplete != nil {
		return 3
	}
	<-served
	return 0
}

func TestSuccessorTakesOverTheSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	file, errFile := listener.(*net.TCPListener).File()
	if errFile != nil {
		t.Fatal(errFile)
	}
	successor, errBegin := Begin(file)
	_ = file.Close()
	if errBegin != nil {
		t.Fatal(errBegin)
	}
	if errReady := successor.Ready(30 * time.Second); errReady != nil {
		t.Fatal(errReady)
	}
	_ = listener.Close()
	if errCommit := successor.Commit(); errCommit != nil {
		t.Fatal(errCommit)
	}

	conn, errDial := net.Dial("tcp", listener.Addr().String())
	if errDial != nil {
		t.Fatalf("socket closed with the old process's copy: %v", errDial)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if line, _ := bufio.NewReader(conn).ReadString('\n'); line != "successor\n" {
		t.Fatalf("answer = %q, want the successor's", line)
	}
}
