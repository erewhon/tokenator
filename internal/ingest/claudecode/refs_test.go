package claudecode

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/erewhon/tokenator/internal/store"
)

func TestDistinctive(t *testing.T) {
	cases := map[string]bool{
		"return":         false, // plain word
		"called":         false,
		"Hello":          false, // capitalized word
		"README":         false, // all caps, no separator
		"abcd":           false, // too short
		"12345":          false, // no letter
		"file_path":      true,  // underscore
		"cmd/tokenator":  true,  // path
		"claude-mem":     true,  // hyphen
		"schemaV4":       true,  // camel transition + digit
		"parseConfig":    true,  // camel transition
		"v1.54":          true,  // digit + dot
		"tokenator.go":   true,  // dotted
		"get_mailboxes":  true,
		"OTelDatapoint":  true, // lower→upper transition at l→D
		"UPPER_SNAKE_OK": true, // underscore qualifies
	}
	for tok, want := range cases {
		if got := distinctive([]byte(tok)); got != want {
			t.Errorf("distinctive(%q) = %v, want %v", tok, got, want)
		}
	}
}

func TestExtractIdentsTrimsAndDedupes(t *testing.T) {
	idents := extractIdents([]byte("see tokenator.go. then tokenator.go again -flag- /abs/path/x.txt"), 10)
	want := map[string]bool{"tokenator.go": true, "abs/path/x.txt": true, "x.txt": true}
	if len(idents) != len(want) {
		t.Fatalf("idents = %v, want keys %v", idents, want)
	}
	for _, id := range idents {
		if !want[id] {
			t.Errorf("unexpected ident %q", id)
		}
	}
}

// track is a test harness: registers each result's content, plays sources
// in order, and returns per-result verdicts.
func track(t *testing.T, results []string, sources []string) []int64 {
	t.Helper()
	rt := newRefTracker()
	blocks := make([]pendingBlock, len(results))
	for i, content := range results {
		rt.addResult(i, []byte(content))
	}
	for i, src := range sources {
		rt.observeText(fmt.Sprintf("ts%d", i), []byte(src))
	}
	rt.finalize(blocks)
	out := make([]int64, len(blocks))
	for i := range blocks {
		out[i] = blocks[i].blk.Referenced
	}
	return out
}

func TestTrackerVerdicts(t *testing.T) {
	got := track(t,
		[]string{
			"func parseConfig() in config_loader.go",  // both idents cited → referenced
			"lonelyFunction defined in unused_helper", // never cited → unreferenced
			"ok done", // no identifiers → unknown
		},
		[]string{"I'll call parseConfig from config_loader.go"},
	)
	if got[0] != store.RefReferenced {
		t.Errorf("result 0 = %d, want referenced", got[0])
	}
	if got[1] != store.RefUnreferenced {
		t.Errorf("result 1 = %d, want unreferenced", got[1])
	}
	if got[2] != store.RefUnknown {
		t.Errorf("result 2 = %d, want unknown (no identifiers)", got[2])
	}
}

func TestSingleIdentifierNeedsOne(t *testing.T) {
	got := track(t,
		[]string{"the only token is special_thing here"},
		[]string{"about special_thing"},
	)
	if got[0] != store.RefReferenced {
		t.Errorf("single-ident result = %d, want referenced on one hit", got[0])
	}
}

func TestIdentifierFiresOnce(t *testing.T) {
	// The identifier is retired on first source hit; a second mention must
	// not push a two-ident result over the line. Partial evidence (1 of 2)
	// abstains rather than calling the result unreferenced.
	got := track(t,
		[]string{"needs both shared_thing and other_thing"},
		[]string{"mentions shared_thing", "mentions shared_thing again"},
	)
	if got[0] != store.RefUnknown {
		t.Errorf("verdict = %d, want unknown (partial evidence abstains)", got[0])
	}
}

func TestAmbientIdentifierTombstoned(t *testing.T) {
	// common_root is claimed by more than maxBlocksPerIdent results →
	// ambient; citing it credits no one.
	results := make([]string, maxBlocksPerIdent+1)
	for i := range results {
		results[i] = fmt.Sprintf("common_root plus unique_sym%dx", i)
	}
	got := track(t, results, []string{"talking about common_root only"})
	for i, v := range got {
		if v != store.RefUnreferenced {
			t.Errorf("result %d = %d, want unreferenced (ambient ident must not credit)", i, v)
		}
	}
}

func TestToolInputValuesOnlyKeysIgnored(t *testing.T) {
	rt := newRefTracker()
	blocks := make([]pendingBlock, 2)
	rt.addResult(0, []byte("mentions old_string and new_string fields")) // idents match JSON KEYS below
	rt.addResult(1, []byte("content of config_loader.go with parseConfig"))
	rt.observeToolInput("ts1", json.RawMessage(
		`{"old_string":"x","new_string":"y","file_path":"/p/config_loader.go","note":"calls parseConfig"}`))
	rt.finalize(blocks)
	if blocks[0].blk.Referenced != store.RefUnreferenced {
		t.Errorf("key-named result = %d, want unreferenced (JSON keys are not sources)", blocks[0].blk.Referenced)
	}
	if blocks[1].blk.Referenced != store.RefReferenced {
		t.Errorf("value-cited result = %d, want referenced (path + symbol in values)", blocks[1].blk.Referenced)
	}
}
