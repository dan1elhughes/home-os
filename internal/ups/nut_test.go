package ups

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeNUT announces which NUT commands it saw and what to reply with.
// No mocks: the client talks to this over a real TCP socket.
type fakeNUT struct {
	t   *testing.T
	ln  net.Listener
	cmd []string // commands received, in order

	// reply script: value of ups.status for GET VAR, and whether
	// credentials are rejected.
	status string
	reject bool
}

func startFakeNUT(t *testing.T, status string) *fakeNUT {
	t.Helper()
	f := &fakeNUT{status: status}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeNUT) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if f.reject {
			fmt.Fprintln(conn, "ERR ACCESS-DENIED")
			continue
		}
		switch {
		case strings.HasPrefix(line, "USERNAME "):
			f.cmd = append(f.cmd, "USERNAME")
			fmt.Fprintln(conn, "OK Username specified")
		case strings.HasPrefix(line, "PASSWORD "):
			f.cmd = append(f.cmd, "PASSWORD")
			fmt.Fprintln(conn, "OK Password specified")
		case strings.HasPrefix(line, "LOGIN "):
			f.cmd = append(f.cmd, "LOGIN")
			fmt.Fprintln(conn, "OK Logged in")
		case strings.HasPrefix(line, "GET VAR "):
			f.cmd = append(f.cmd, "GET "+strings.Fields(line)[3])
			fmt.Fprintf(conn, "VAR ups ups.status \"%s\"\n", f.status)
		case strings.HasPrefix(line, "LOGOUT"):
			return
		default:
			fmt.Fprintln(conn, "ERR UNKNOWN-COMMAND")
		}
	}
}

func TestNUTLoginAndStatus(t *testing.T) {
	f := startFakeNUT(t, "OB DISCHRG")
	c, err := Dial(f.ln.Addr().String(), "watcher", "pw", "ups", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	st, err := c.Status()
	if err != nil {
		t.Fatal(err)
	}
	if st != "OB DISCHRG" {
		t.Fatalf("status = %q, want %q", st, "OB DISCHRG")
	}
	if got, want := strings.Join(f.cmd, ","), "USERNAME,PASSWORD,LOGIN,GET ups.status"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}

	// A second status read on the same connection must also work.
	if st, err := c.Status(); err != nil || st != "OB DISCHRG" {
		t.Fatalf("second status: %q err=%v", st, err)
	}
	if got, want := strings.Join(f.cmd, ","), "USERNAME,PASSWORD,LOGIN,GET ups.status,GET ups.status"; got != want {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestNUTFailureIsError(t *testing.T) {
	f := startFakeNUT(t, "")
	f.reject = true
	if c, err := Dial(f.ln.Addr().String(), "w", "bad", "ups", 2*time.Second); err == nil {
		c.Close()
		t.Fatalf("want error for ERR reply, got client %+v", c)
	} else if !strings.Contains(err.Error(), "ERR") {
		t.Fatalf("error should carry the NUT ERR code, got %v", err)
	}

	// Connect refusal: nothing listening anymore.
	if _, err := Dial("127.0.0.1:1", "w", "pw", "ups", 200*time.Millisecond); err == nil {
		t.Fatal("want error for refused connect")
	}
}

// Anonymous read: pi-nut serves GET VAR without credentials (that is how
// the HA integration reads it). Dial with empty user/pass must skip the
// login handshake entirely.
func TestNUTAnonymousDial(t *testing.T) {
	f := startFakeNUT(t, "OL CHRG")
	c, err := Dial(f.ln.Addr().String(), "", "", "ups", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if st, err := c.Status(); err != nil || st != "OL CHRG" {
		t.Fatalf("status = %q err=%v", st, err)
	}
	if got, want := strings.Join(f.cmd, ","), "GET ups.status"; got != want {
		t.Fatalf("commands = %q, want %q (no login)", got, want)
	}
}
