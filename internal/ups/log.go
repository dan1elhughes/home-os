package ups

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EventLine is one log-contract record; its key names are fixed.
type EventLine struct {
	TS     string `json:"ts"`
	Node   string `json:"node"`
	Event  string `json:"event"`
	Detail string `json:"detail"`
}

// RotateBytes is the size-guard threshold: past it the log rotates to
// <name>.1 (one generation kept), never truncated, so history survives.
const RotateBytes = 10 << 20

// Logger appends EventLines to a file and mirrors them to stdout.
// Safe for concurrent use.
type Logger struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	nodeID string
}

// OpenLogger opens (or rotates) the log file and returns a Logger that
// writes to it and to stdout. A broken file side never fails the daemon;
// stdout always works.
func OpenLogger(path, nodeID string) (*Logger, error) {
	l := &Logger{path: path, nodeID: nodeID}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// OpenLoggerStdout returns a Logger that only mirrors to stdout (used
// when the log file's volume is unavailable; the daemon keeps running).
func OpenLoggerStdout(nodeID string) *Logger {
	return &Logger{nodeID: nodeID}
}

func (l *Logger) open() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	if st, err := os.Stat(l.path); err == nil && st.Size() > RotateBytes {
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	l.f = f
	return nil
}

// Log writes one event line to the file and stdout. detail may be empty.
func (l *Logger) Log(event, detail string) {
	line := EventLine{
		TS:     time.Now().UTC().Format(time.RFC3339),
		Node:   l.nodeID,
		Event:  event,
		Detail: detail,
	}
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	out := string(b) + "\n"

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		if _, err := l.f.WriteString(out); err != nil && l.f.Close() == nil {
			// Reopen once; if the volume came back, continue appending.
			if err2 := l.open(); err2 == nil {
				l.f.WriteString(out)
			}
		}
	}
	fmt.Fprint(os.Stdout, out)
}
