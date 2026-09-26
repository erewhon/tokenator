// Package transcript reads session content back out of the harness's own
// files for the browser/search UI. The database deliberately stores no block
// content (sizes and hashes only), so this package is the display/search
// path: the store narrows *which* sessions to look at, and transcript reads
// the underlying files on demand.
//
// Layouts (mirroring the ingesters):
//
//	claude_code: <root>/<project-slug>/<session-uuid>.jsonl (one line per event)
//	opencode:    <root>/message/<ses_*>/<msg_*>.json + <root>/part/<msg_*>/prt_*.json
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one displayable transcript item.
type Entry struct {
	Idx     int    // ordinal within the session view (anchor target)
	TS      string // RFC3339 when known
	Role    string // user | assistant | system
	Kind    string // user_text | meta_text | assistant_text | thinking | tool_use | tool_result | image | compaction
	Tool    string // tool name for tool_use/tool_result
	Text    string // full content text ('' for images)
	IsError bool
}

// Locate returns the files holding a session's content, or an error when
// none exist (e.g. transcript deleted or on another machine).
func Locate(sourceKind, root, sessionKey string) ([]string, error) {
	switch sourceKind {
	case "claude_code":
		files, err := filepath.Glob(filepath.Join(root, "*", sessionKey+".jsonl"))
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("no transcript file for session %s under %s", sessionKey, root)
		}
		sort.Strings(files)
		return files, nil
	case "opencode":
		if isOpenCodeDB(root) {
			return locateOpenCodeDB(root, sessionKey)
		}
		dir := filepath.Join(root, "message", sessionKey)
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("no message dir for session %s under %s", sessionKey, root)
		}
		return []string{dir}, nil
	default:
		return nil, fmt.Errorf("source kind %q has no transcript files", sourceKind)
	}
}

// Load parses a session's transcript into display entries.
func Load(sourceKind, root, sessionKey string) ([]Entry, error) {
	files, err := Locate(sourceKind, root, sessionKey)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	switch sourceKind {
	case "claude_code":
		for _, f := range files {
			es, err := loadClaudeFile(f)
			if err != nil {
				return nil, err
			}
			entries = append(entries, es...)
		}
	case "opencode":
		if isOpenCodeDB(root) {
			entries, err = loadOpenCodeDBSession(root, sessionKey)
		} else {
			entries, err = loadOpenCodeSession(root, sessionKey)
		}
		if err != nil {
			return nil, err
		}
	}
	for i := range entries {
		entries[i].Idx = i
	}
	return entries, nil
}

// --- Claude Code ---

// ccLine is the subset of transcript line fields the viewer needs.
type ccLine struct {
	Type        string          `json:"type"`
	Timestamp   string          `json:"timestamp"`
	IsMeta      bool            `json:"isMeta"`
	Subtype     string          `json:"subtype"`
	Message     json.RawMessage `json:"message"`
	CompactMeta *struct {
		Trigger   string `json:"trigger"`
		PreTokens int64  `json:"preTokens"`
	} `json:"compactMetadata"`
}

type ccMessage struct {
	Content json.RawMessage `json:"content"` // string or []ccBlock
}

type ccBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`   // tool_use: toolu_* id
	Name      string          `json:"name"` // tool_use: tool name
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"` // tool_result: back-reference
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// maxLineBytes matches the ingester's tolerance for huge tool-result lines.
const maxLineBytes = 64 * 1024 * 1024

func loadClaudeFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), maxLineBytes)
	// toolByID maps tool_use ids to names so results display their tool.
	toolByID := map[string]string{}
	for sc.Scan() {
		out = append(out, parseClaudeLine(sc.Bytes(), toolByID)...)
	}
	return out, sc.Err()
}

// parseClaudeLine turns one transcript line into zero or more entries.
func parseClaudeLine(raw []byte, toolByID map[string]string) []Entry {
	var ln ccLine
	if err := json.Unmarshal(raw, &ln); err != nil {
		return nil
	}
	switch ln.Type {
	case "system":
		if ln.Subtype == "compact_boundary" {
			e := Entry{TS: ln.Timestamp, Role: "system", Kind: "compaction"}
			if ln.CompactMeta != nil {
				e.Text = fmt.Sprintf("compaction (%s, pre: %d tokens)", ln.CompactMeta.Trigger, ln.CompactMeta.PreTokens)
			} else {
				e.Text = "compaction"
			}
			return []Entry{e}
		}
		return nil
	case "user", "assistant":
	default:
		return nil
	}
	var msg ccMessage
	if err := json.Unmarshal(ln.Message, &msg); err != nil || len(msg.Content) == 0 {
		return nil
	}
	role := ln.Type
	// content may be a bare string (plain user turns)...
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return []Entry{{TS: ln.Timestamp, Role: role, Kind: textKind(role, ln.IsMeta), Text: s}}
	}
	// ...or a block array.
	var blocks []ccBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil
	}
	var out []Entry
	for _, b := range blocks {
		e := Entry{TS: ln.Timestamp, Role: role}
		switch b.Type {
		case "text":
			e.Kind, e.Text = textKind(role, ln.IsMeta), b.Text
		case "thinking":
			e.Kind, e.Text = "thinking", b.Thinking
		case "tool_use":
			e.Kind, e.Tool = "tool_use", b.Name
			e.Text = compactJSON(b.Input)
			if b.ID != "" {
				toolByID[b.ID] = b.Name
			}
		case "tool_result":
			e.Kind, e.IsError = "tool_result", b.IsError
			e.Tool = toolByID[b.ToolUseID]
			e.Text = resultText(b.Content)
		case "image":
			e.Kind, e.Text = "image", "[image]"
		default:
			continue
		}
		out = append(out, e)
	}
	return out
}

func textKind(role string, isMeta bool) string {
	switch {
	case role == "assistant":
		return "assistant_text"
	case isMeta:
		return "meta_text"
	default:
		return "user_text"
	}
}

// resultText flattens a tool_result payload: bare string, or a block array
// whose text members are concatenated.
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []ccBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

// compactJSON pretty-prints a tool input for display.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// --- OpenCode ---

type ocMessage struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Time struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

type ocPart struct {
	Type  string `json:"type"` // text | reasoning | tool | step-start | step-finish | patch
	Text  string `json:"text"`
	Tool  string `json:"tool"`
	State *struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output json.RawMessage `json:"output"`
	} `json:"state"`
}

// entriesFromOCPart maps one OpenCode part to display entries: text and
// reasoning are one entry each; a tool part carries both the call and its
// result, so it yields up to two. Bookkeeping parts (step-start,
// step-finish, patch, compaction) yield none.
func entriesFromOCPart(p *ocPart, role, ts string) []Entry {
	e := Entry{TS: ts, Role: role}
	switch p.Type {
	case "text":
		e.Kind, e.Text = textKind(role, false), p.Text
	case "reasoning":
		e.Kind, e.Text = "thinking", p.Text
	case "tool":
		e.Kind, e.Tool = "tool_use", p.Tool
		if p.State != nil {
			e.Text = compactJSON(p.State.Input)
		}
		out := []Entry{e}
		if p.State != nil && len(p.State.Output) > 0 {
			out = append(out, Entry{TS: ts, Role: role, Kind: "tool_result",
				Tool: p.Tool, IsError: p.State.Status == "error",
				Text: resultText(p.State.Output)})
		}
		return out
	default:
		return nil
	}
	return []Entry{e}
}

func loadOpenCodeSession(root, sessionKey string) ([]Entry, error) {
	msgPaths, err := filepath.Glob(filepath.Join(root, "message", sessionKey, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(msgPaths) // msg_* ids are k-sortable → chronological
	var out []Entry
	for _, mp := range msgPaths {
		data, err := os.ReadFile(mp)
		if err != nil {
			continue
		}
		var m ocMessage
		if err := json.Unmarshal(data, &m); err != nil || m.ID == "" {
			continue
		}
		ts := ""
		if m.Time.Created > 0 {
			ts = time.UnixMilli(m.Time.Created).UTC().Format(time.RFC3339)
		}
		partPaths, _ := filepath.Glob(filepath.Join(root, "part", m.ID, "*.json"))
		sort.Strings(partPaths)
		for _, pp := range partPaths {
			pdata, err := os.ReadFile(pp)
			if err != nil {
				continue
			}
			var p ocPart
			if err := json.Unmarshal(pdata, &p); err != nil {
				continue
			}
			out = append(out, entriesFromOCPart(&p, m.Role, ts)...)
		}
	}
	return out, nil
}
