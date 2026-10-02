package ups

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// defaultTimeout bounds each status reply read in the daemon loop. Dial
// callers pass their own value for the login handshake.
const defaultTimeout = 5 * time.Second

// Client is a logged-in NUT session over one TCP connection.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
	ups  string
}

// Dial connects to the NUT server (host:port), logs in with
// USERNAME/PASSWORD/LOGIN, and leaves the connection ready for Status.
// timeout bounds the connect and every reply read.
func Dial(addr, user, pass, ups string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("nutsrv %s: %w", addr, err)
	}
	c := &Client{conn: conn, r: bufio.NewReader(conn), ups: ups}
	// Credentials are optional: pi-nut serves read-only GET VARs to any
	// LAN client (that is how the HA integration reads it). Only handshake
	// when one side asked for it.
	if user != "" || pass != "" {
		if _, err := c.sendRead("USERNAME "+user, timeout); err != nil {
			conn.Close()
			return nil, fmt.Errorf("nutsrv %s: %w", addr, err)
		}
		if _, err := c.sendRead("PASSWORD "+pass, timeout); err != nil {
			conn.Close()
			return nil, fmt.Errorf("nutsrv %s: %w", addr, err)
		}
		if _, err := c.sendRead("LOGIN "+ups, timeout); err != nil {
			conn.Close()
			return nil, fmt.Errorf("nutsrv %s: %w", addr, err)
		}
	}
	return c, nil
}

// Status returns the current ups.status value, e.g. "OL CHRG".
func (c *Client) Status() (string, error) {
	line, err := c.sendRead("GET VAR "+c.ups+" ups.status", defaultTimeout)
	if err != nil {
		return "", err
	}
	return parseVarReply(c.ups, line)
}

// Close closes the connection. NUT wants LOGOUT first; best effort, the
// server also closes on its side once we drop the socket.
func (c *Client) Close() error {
	fmt.Fprintln(c.conn, "LOGOUT")
	return c.conn.Close()
}

func (c *Client) sendRead(cmd string, timeout time.Duration) (string, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", err
	}
	if _, err := io.WriteString(c.conn, cmd+"\n"); err != nil {
		return "", err
	}
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("%s: %w", cmd, err)
	}
	line = strings.TrimRight(line, "\r\n")
	if strings.HasPrefix(line, "ERR ") {
		return "", fmt.Errorf("%s: %s", cmd, line)
	}
	return line, nil
}

// parseVarReply extracts the quoted value from a VAR reply line:
//
//	VAR ups ups.status "OL CHRG"
func parseVarReply(ups, line string) (string, error) {
	prefix := "VAR " + ups + " ups.status \""
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "\"") {
		return "", fmt.Errorf("unexpected VAR reply: %q", line)
	}
	v := line[len(prefix) : len(line)-1]
	return unescapeNUT(v), nil
}

// unescapeNUT resolves the two escapes the NUT protocol defines.
func unescapeNUT(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == '\\' {
				b.WriteByte('\\')
				continue
			}
			if s[i] == '"' {
				b.WriteByte('"')
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
