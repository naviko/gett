package ftpx

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// silentServer speaks just enough FTP to accept a listing request and then
// never sends the listing or closes the data connection.
func silentServer(t *testing.T) string {
	t.Helper()
	ctl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	data, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctl.Close(); data.Close() })
	port := data.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := data.Accept()
			if err != nil {
				return
			}
			defer c.Close() // held open, nothing sent
		}
	}()
	go func() {
		for {
			c, err := ctl.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				fmt.Fprintf(c, "220 silent\r\n")
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.ToUpper(strings.Fields(line)[0])
					switch cmd {
					case "USER":
						fmt.Fprintf(c, "230 ok\r\n")
					case "FEAT":
						fmt.Fprintf(c, "211-Features:\r\n MLST type*;size*;\r\n211 End\r\n")
					case "TYPE":
						fmt.Fprintf(c, "200 ok\r\n")
					case "EPSV":
						fmt.Fprintf(c, "502 no\r\n")
					case "PASV":
						fmt.Fprintf(c, "227 Entering Passive Mode (127,0,0,1,%d,%d)\r\n", port/256, port%256)
					case "MLSD", "LIST":
						fmt.Fprintf(c, "150 here it comes\r\n") // and then nothing, ever
					case "QUIT":
						fmt.Fprintf(c, "221 bye\r\n")
						return
					default:
						fmt.Fprintf(c, "502 no\r\n")
					}
				}
			}()
		}
	}()
	return ctl.Addr().String()
}

func TestSilentServerDoesNotHang(t *testing.T) {
	addr := silentServer(t)
	url := "ftp://" + addr + "/pub/"

	// Ctrl-C ends a listing the server never finishes.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- List(ctx, url, &strings.Builder{}) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want interrupted, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listing kept waiting after Ctrl-C")
	}

	// Without Ctrl-C the stall timeout ends it.
	old := stallTimeout
	stallTimeout = 300 * time.Millisecond
	defer func() { stallTimeout = old }()
	go func() { done <- List(context.Background(), url, &strings.Builder{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("listing from a silent server succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listing from a silent server never gave up")
	}
}

// muteServer accepts connections and never says anything, like a crashed
// FTP service whose port is still open.
func muteServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	return l.Addr().String()
}

func TestMuteServerTimesOutAndStops(t *testing.T) {
	addr := muteServer(t)

	// Ctrl-C while waiting for the greeting.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- List(ctx, "ftp://"+addr+"/up/", &strings.Builder{}) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want interrupted, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("kept waiting for a greeting after Ctrl-C")
	}

	// Left alone, it gives up after the stall timeout, once: a server that
	// does not greet over TLS is not asked again in plain FTP.
	old := stallTimeout
	stallTimeout = 300 * time.Millisecond
	defer func() { stallTimeout = old }()
	start := time.Now()
	err := List(context.Background(), "ftp://"+addr+"/", &strings.Builder{})
	if err == nil {
		t.Fatal("a mute server was listed")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s to give up on a mute server", d)
	}
}
