// Stale-passenger reference detection. Block content is never stored, so
// the "was this tool result ever used?" judgment happens here, at ingest,
// while the content is in hand, and only the verdict persists.
//
// Single pass in file order: each tool_result registers a bounded set of
// DISTINCTIVE identifiers (code-shaped tokens — paths, symbols, dotted or
// hyphenated names; plain English words never qualify) into an inverted
// index. Later reference sources — assistant text, thinking, tool_use input
// values, and human (non-meta) user text — consume the index: the first
// occurrence of an identifier credits every result that produced it, then
// retires the identifier. A result is REFERENCED once two distinct
// identifiers hit (one, if it only produced one); an identifier claimed by
// too many results is ambient (repo path prefixes, ubiquitous symbols) and
// credits no one.
//
// The verdict is a heuristic and is labeled "likely" downstream: quoting an
// identifier is not proof the model used the result, and using a result
// without naming anything from it is invisible. Precision matters most on
// the UNREFERENCED side, which the thresholds favor.
package claudecode

import (
	"bytes"
	"encoding/json"

	"github.com/erewhon/tokenator/internal/store"
)

const (
	minIdentLen       = 5
	maxIdentLen       = 80
	maxIdentsPerBlock = 800 // memory bound per tool_result
	maxBlocksPerIdent = 8   // claimed by more results → ambient, retire
	needDistinct      = 2   // matches to call a result referenced
)

type refResult struct {
	blockIdx int // index into fileCtx.blocks
	need     int
	orig     int // need at registration; need < orig means partial evidence
	refTS    string
}

type identEntry struct {
	results []*refResult
	dead    bool
}

type refTracker struct {
	index   map[string]*identEntry
	results []*refResult
}

func newRefTracker() *refTracker {
	return &refTracker{index: map[string]*identEntry{}}
}

// addResult registers a tool_result's identifiers. Results yielding no
// distinctive identifiers stay off the books (verdict remains unknown).
func (rt *refTracker) addResult(blockIdx int, content []byte) {
	idents := extractIdents(content, maxIdentsPerBlock)
	if len(idents) == 0 {
		return
	}
	need := min(needDistinct, len(idents))
	r := &refResult{blockIdx: blockIdx, need: need, orig: need}
	rt.results = append(rt.results, r)
	for _, id := range idents {
		e := rt.index[id]
		if e == nil {
			e = &identEntry{}
			rt.index[id] = e
		}
		if e.dead {
			continue
		}
		if len(e.results) >= maxBlocksPerIdent {
			e.dead = true
			e.results = nil
			continue
		}
		e.results = append(e.results, r)
	}
}

// observeText scans a reference source. Each identifier fires at most once
// across the whole file: first hit credits its producers and retires it.
func (rt *refTracker) observeText(ts string, data []byte) {
	if len(rt.index) == 0 {
		return
	}
	scanTokens(data, func(tok string) bool {
		e, ok := rt.index[tok]
		if !ok {
			return true
		}
		delete(rt.index, tok)
		if e.dead {
			return true
		}
		for _, r := range e.results {
			if r.need == 0 {
				continue
			}
			r.need--
			if r.need == 0 {
				r.refTS = ts
			}
		}
		return true
	})
}

// observeToolInput scans only the string VALUES of a tool input — keys
// ("file_path", "old_string", ...) repeat in every call and would be pure
// noise. Unparseable input falls back to raw-byte scanning.
func (rt *refTracker) observeToolInput(ts string, input json.RawMessage) {
	if len(rt.index) == 0 || len(input) == 0 {
		return
	}
	var v any
	if json.Unmarshal(input, &v) != nil {
		rt.observeText(ts, input)
		return
	}
	rt.observeValues(ts, v)
}

func (rt *refTracker) observeValues(ts string, v any) {
	switch x := v.(type) {
	case string:
		rt.observeText(ts, []byte(x))
	case []any:
		for _, e := range x {
			rt.observeValues(ts, e)
		}
	case map[string]any:
		for _, e := range x {
			rt.observeValues(ts, e)
		}
	}
}

// finalize writes verdicts onto the pending blocks. Zero hits is a clean
// UNREFERENCED; partial evidence (some but not enough identifiers hit)
// abstains — the ambient tombstone can eat a result's other identifiers,
// and a false "unreferenced" is the expensive mistake here.
func (rt *refTracker) finalize(blocks []pendingBlock) (referenced, unreferenced int) {
	for _, r := range rt.results {
		blk := &blocks[r.blockIdx].blk
		switch {
		case r.need == 0:
			blk.Referenced = store.RefReferenced
			blk.FirstRefTS = r.refTS
			referenced++
		case r.need == r.orig:
			blk.Referenced = store.RefUnreferenced
			unreferenced++
		}
	}
	return referenced, unreferenced
}

// --- identifier extraction ---

func isTokChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' || c == '_' || c == '.' || c == '/' || c == '-'
}

// scanTokens walks maximal [A-Za-z0-9_./-]+ runs, trims sentence punctuation
// off the ends, and yields the distinctive ones. Path-shaped tokens also
// yield their basename: the same file is spelled with different prefixes on
// the two sides (a grep result's "src/config_loader.go" vs an edit input's
// absolute path), and the basename is the stable part. Return false to stop.
func scanTokens(data []byte, yield func(string) bool) {
	i := 0
	for i < len(data) {
		if !isTokChar(data[i]) {
			i++
			continue
		}
		j := i
		for j < len(data) && isTokChar(data[j]) {
			j++
		}
		tok := trimEnds(data[i:j])
		i = j
		if distinctive(tok) && !yield(string(tok)) {
			return
		}
		if k := bytes.LastIndexByte(tok, '/'); k >= 0 {
			if base := tok[k+1:]; distinctive(base) && !yield(string(base)) {
				return
			}
		}
	}
}

// trimEnds strips separators that are sentence punctuation rather than part
// of the identifier ("tokenator." at the end of a sentence, "-" bullets).
func trimEnds(tok []byte) []byte {
	for len(tok) > 0 && (tok[0] == '.' || tok[0] == '/' || tok[0] == '-') {
		tok = tok[1:]
	}
	for len(tok) > 0 {
		c := tok[len(tok)-1]
		if c != '.' && c != '/' && c != '-' {
			break
		}
		tok = tok[:len(tok)-1]
	}
	return tok
}

// distinctive reports whether a token looks like a code identifier or path
// rather than prose: it must contain a letter plus a separator, a digit, or
// an internal case transition. "return", "ERROR", "Hello" fail; "file_path",
// "cmd/tokenator", "schemaV4", "claude-mem", "v1.54" pass.
func distinctive(tok []byte) bool {
	if len(tok) < minIdentLen || len(tok) > maxIdentLen {
		return false
	}
	hasLetter, codeLike := false, false
	for i, c := range tok {
		switch {
		case c == '_' || c == '.' || c == '/' || c == '-' || c >= '0' && c <= '9':
			codeLike = true
		case c >= 'a' && c <= 'z':
			hasLetter = true
		case c >= 'A' && c <= 'Z':
			hasLetter = true
			if i > 0 && tok[i-1] >= 'a' && tok[i-1] <= 'z' {
				codeLike = true // camelCase transition
			}
		}
	}
	return hasLetter && codeLike
}

// extractIdents returns up to max distinct distinctive identifiers.
func extractIdents(data []byte, max int) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 64)
	scanTokens(data, func(tok string) bool {
		if _, dup := seen[tok]; dup {
			return true
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
		return len(out) < max
	})
	return out
}
