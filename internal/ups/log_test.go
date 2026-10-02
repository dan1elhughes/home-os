package ups

import (
	"encoding/json"
	"strings"
	"testing"
)

// The log contract: one JSON object per line, keys ts/node/event/detail,
// appended to UPS_LOG and mirrored to stdout for journald.
func TestEventLineJSON(t *testing.T) {
	b, err := json.Marshal(EventLine{TS: "2026-10-02T23:10:05Z", Node: "cl02", Event: "DRAIN_START", Detail: "availability=drain"})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	want := `{"ts":"2026-10-02T23:10:05Z","node":"cl02","event":"DRAIN_START","detail":"availability=drain"}`
	if got != want {
		t.Fatalf("json = %s\nwant    %s", got, want)
	}

	// A detail containing quotes must round-trip as valid JSON, not break out.
	b, _ = json.Marshal(EventLine{Event: "X", Detail: `weird "quoted" value`})
	if !json.Valid(b) || !strings.Contains(string(b), `weird \"quoted\" value`) {
		t.Fatalf("quote handling broken: %s", b)
	}
}
