package transcript

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// Hit is one matching entry within a session.
type Hit struct {
	EntryIdx int // anchor into the Load() ordering
	TS       string
	Kind     string
	Tool     string
	Snippet  string
}

// SessionHits summarizes a content search within one session.
type SessionHits struct {
	Matches int // total matching entries (may exceed len(Hits))
	Hits    []Hit
}

// Search scans one session's transcript for a case-insensitive substring.
// A cheap raw-byte pass skips non-matching files entirely; only files that
// might match are fully parsed (so snippets and anchors line up with Load).
func Search(sourceKind, root, sessionKey, query string, maxHits int) (SessionHits, error) {
	var out SessionHits
	if query == "" {
		return out, nil
	}
	if fastPathOK(query) {
		match, err := rawMatch(sourceKind, root, sessionKey, query)
		if err != nil || !match {
			return out, err
		}
	}
	entries, err := Load(sourceKind, root, sessionKey)
	if err != nil {
		return out, err
	}
	lq := strings.ToLower(query)
	for _, e := range entries {
		pos := strings.Index(strings.ToLower(e.Text), lq)
		if pos < 0 {
			continue
		}
		out.Matches++
		if len(out.Hits) < maxHits {
			out.Hits = append(out.Hits, Hit{
				EntryIdx: e.Idx, TS: e.TS, Kind: e.Kind, Tool: e.Tool,
				Snippet: snippet(e.Text, pos, len(query)),
			})
		}
	}
	return out, nil
}

// fastPathOK reports whether the query can be searched in raw JSON bytes
// without false negatives. JSON escapes quotes, backslashes, control chars,
// and (in some encoders) <, >, &, and non-ASCII — queries containing any of
// those must skip the raw pass and go straight to parsing.
func fastPathOK(q string) bool {
	for _, r := range q {
		if r < 0x20 || r > 0x7e || strings.ContainsRune(`"\<>&`, r) {
			return false
		}
	}
	return true
}

// rawMatch reports whether any of the session's files contain the query,
// case-insensitively, without JSON parsing.
func rawMatch(sourceKind, root, sessionKey, query string) (bool, error) {
	files, err := Locate(sourceKind, root, sessionKey)
	if err != nil {
		return false, err
	}
	needle := []byte(strings.ToLower(query))
	switch sourceKind {
	case "claude_code":
		for _, f := range files {
			ok, err := fileContains(f, needle)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case "opencode":
		// Messages carry no content; scan the session's part files.
		msgPaths, err := filepath.Glob(filepath.Join(root, "message", sessionKey, "*.json"))
		if err != nil {
			return false, err
		}
		for _, mp := range msgPaths {
			msgID := strings.TrimSuffix(filepath.Base(mp), ".json")
			parts, _ := filepath.Glob(filepath.Join(root, "part", msgID, "*.json"))
			for _, pp := range parts {
				ok, err := fileContains(pp, needle)
				if err != nil {
					continue
				}
				if ok {
					return true, nil
				}
			}
		}
		return false, nil
	}
	return false, nil
}

// fileContains streams a file looking for a lowercase needle.
func fileContains(path string, needle []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	// Overlapping chunk scan: keep len(needle)-1 bytes across boundaries.
	keep := len(needle) - 1
	buf := make([]byte, 1<<20+keep)
	carry := 0
	for {
		n, err := r.Read(buf[carry:])
		if n > 0 {
			chunk := bytes.ToLower(buf[:carry+n])
			if bytes.Contains(chunk, needle) {
				return true, nil
			}
			if keep > 0 && carry+n > keep {
				copy(buf, buf[carry+n-keep:carry+n])
				carry = keep
			} else {
				carry = carry + n
			}
		}
		if err != nil {
			return false, nil
		}
	}
}

// snippet extracts a whitespace-collapsed window around a match.
func snippet(text string, pos, matchLen int) string {
	const ctx = 90
	start := pos - ctx
	if start < 0 {
		start = 0
	}
	end := pos + matchLen + ctx
	if end > len(text) {
		end = len(text)
	}
	// Don't split multi-byte runes at the window edges.
	for start > 0 && start < len(text) && (text[start]&0xc0) == 0x80 {
		start++
	}
	for end < len(text) && (text[end]&0xc0) == 0x80 {
		end++
	}
	s := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}
