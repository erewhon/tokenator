// Package claudecode ingests Claude Code transcript files
// (~/.claude/projects/<project-slug>/<session-uuid>.jsonl) into the store.
//
// The format is an append-only JSONL event log whose schema drifts across
// Claude Code versions, so parsing is deliberately lenient: unknown line
// types and unknown fields are ignored, and a malformed line is counted
// rather than fatal.
//
// Facts this ingester relies on, verified empirically (2026-07, CC ~2.x
// transcripts; see docs/phase1-profiler.md):
//
//   - Assistant messages are written as one line PER CONTENT BLOCK, each
//     repeating the same message.id, requestId, and identical usage. Summing
//     naively overcounts ~2x. The same (message.id, requestId) pair also
//     appears in MULTIPLE FILES when sessions are resumed/forked across
//     project directories, so deduplication must be global (dedupe_key is
//     UNIQUE across the whole request table).
//   - usage carries input_tokens, output_tokens, cache_creation_input_tokens,
//     cache_read_input_tokens, plus a cache_creation object with the
//     ephemeral_5m/1h TTL split (needed later for 1.25x vs 2x write pricing).
//   - Synthetic assistant lines (model == "<synthetic>") are harness-injected
//     messages, not API calls; they are skipped.
//   - Compactions appear as system lines with subtype "compact_boundary" and
//     compactMetadata {trigger, preTokens}.
//   - Session titles arrive on dedicated "ai-title"/"custom-title" lines;
//     subagent sessions are named by "agent-name" lines.
package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/erewhon/tokenator/internal/estimate"
	"github.com/erewhon/tokenator/internal/store"
)

const (
	SourceKind = "claude_code"
	provider   = "anthropic"
)

type Ingester struct {
	// Root is the projects directory; empty means ~/.claude/projects.
	Root string
	// Regime is the billing regime recorded on the source row.
	// Claude Code on a Max/Pro subscription is "subscription".
	Regime string
	// Full re-parses every file regardless of recorded size/mtime.
	// Needed once after a schema migration adds new extraction (idempotent
	// thanks to global dedupe keys).
	Full bool
}

type Stats struct {
	Files         int
	FilesSkipped  int
	Lines         int
	ParseErrors   int
	Sessions      int
	Requests      int
	Duplicates    int
	Synthetic     int
	Compactions   int
	Blocks        int
	BlocksUpdated int
}

func (st Stats) String() string {
	return fmt.Sprintf(
		"files=%d (skipped %d unchanged) lines=%d parse_errors=%d sessions=%d requests=+%d dup=%d synthetic=%d compactions=%d blocks=+%d/%d",
		st.Files, st.FilesSkipped, st.Lines, st.ParseErrors, st.Sessions,
		st.Requests, st.Duplicates, st.Synthetic, st.Compactions,
		st.Blocks, st.BlocksUpdated)
}

// line is the union of the top-level transcript fields we care about.
type line struct {
	Type          string          `json:"type"`
	UUID          string          `json:"uuid"`
	SessionID     string          `json:"sessionId"`
	Timestamp     string          `json:"timestamp"`
	CWD           string          `json:"cwd"`
	Slug          string          `json:"slug"`
	RequestID     string          `json:"requestId"`
	Subtype       string          `json:"subtype"`
	IsMeta        bool            `json:"isMeta"`
	PromptID      string          `json:"promptId"`
	Message       json.RawMessage `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	AITitle       string          `json:"aiTitle"`
	CustomTitle   string          `json:"customTitle"`
	AgentName     string          `json:"agentName"`
	CompactMeta   *struct {
		Trigger   string `json:"trigger"`
		PreTokens int64  `json:"preTokens"`
	} `json:"compactMetadata"`
}

type assistantMessage struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
	Usage      *usage          `json:"usage"`
}

type userMessage struct {
	Content json.RawMessage `json:"content"` // string or []contentBlock
}

// contentBlock is the union of Anthropic content block fields we read.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`   // tool_use: toolu_* id
	Name      string          `json:"name"` // tool_use: tool name
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"` // tool_result: back-reference
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"` // tool_result payload
}

// toolInput is the union of file-path spellings across built-in tools.
type toolInput struct {
	FilePath     string `json:"file_path"`
	FilePathAlt  string `json:"filePath"`
	Path         string `json:"path"`
	NotebookPath string `json:"notebook_path"`
}

func (ti toolInput) path() string {
	for _, p := range []string{ti.FilePath, ti.FilePathAlt, ti.Path, ti.NotebookPath} {
		if p != "" {
			return p
		}
	}
	return ""
}

// toolUseResult is the sidecar metadata Claude Code attaches to tool-result
// user lines; used only as a file-path fallback.
type toolUseResultMeta struct {
	FilePath string `json:"filePath"`
	File     *struct {
		FilePath string `json:"filePath"`
	} `json:"file"`
}

// mcpServer extracts the server name from an mcp__<server>__<tool> name.
func mcpServer(tool string) string {
	if rest, ok := strings.CutPrefix(tool, "mcp__"); ok {
		if server, _, ok := strings.Cut(rest, "__"); ok {
			return server
		}
	}
	return ""
}

type usage struct {
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	ServiceTier              string `json:"service_tier"`
	Speed                    string `json:"speed"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// per-session metadata accumulated while scanning one file.
type sessAgg struct {
	slug, cwd            string
	aiTitle, customTitle string
	agent                string
	minTS, maxTS         string
}

func (sa *sessAgg) observe(ln *line) {
	if ln.Timestamp != "" {
		if sa.minTS == "" || ln.Timestamp < sa.minTS {
			sa.minTS = ln.Timestamp
		}
		if ln.Timestamp > sa.maxTS {
			sa.maxTS = ln.Timestamp
		}
	}
	if sa.cwd == "" && ln.CWD != "" {
		sa.cwd = ln.CWD
	}
	if sa.slug == "" && ln.Slug != "" {
		sa.slug = ln.Slug
	}
}

func (sa *sessAgg) title() string {
	if sa.customTitle != "" {
		return sa.customTitle
	}
	return sa.aiTitle
}

type pendingRequest struct {
	sessionKey string
	req        store.Request
}

type pendingCompaction struct {
	sessionKey string
	comp       store.Compaction
}

type pendingBlock struct {
	sessionKey string
	blk        store.Block
}

// toolMeta lets tool_result blocks inherit attribution from the tool_use
// that requested them (results carry only a tool_use_id back-reference).
type toolMeta struct {
	tool string
	mcp  string
	file string
}

// fileCtx accumulates everything parsed from one transcript file before the
// write transaction. toolUses resolves within the file, which also covers
// resumed sessions: history is copied wholesale, so a result's tool_use is
// almost always in the same file.
type fileCtx struct {
	fileStem    string
	sessions    map[string]*sessAgg
	requests    []pendingRequest
	compactions []pendingCompaction
	blocks      []pendingBlock
	toolUses    map[string]toolMeta
	stats       *Stats
}

func (fc *fileCtx) session(key string) *sessAgg {
	sa := fc.sessions[key]
	if sa == nil {
		sa = &sessAgg{}
		fc.sessions[key] = sa
	}
	return sa
}

func (fc *fileCtx) addBlock(sessionKey string, blk store.Block) {
	if blk.ByteLen == 0 {
		return
	}
	blk.EstTokens = estimate.Tokens(int(blk.ByteLen))
	fc.blocks = append(fc.blocks, pendingBlock{sessionKey: sessionKey, blk: blk})
}

func (ing *Ingester) root() (string, error) {
	if ing.Root != "" {
		return ing.Root, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

func (ing *Ingester) regime() string {
	if ing.Regime != "" {
		return ing.Regime
	}
	return "subscription"
}

// Run discovers and ingests all transcript files under the root. Files whose
// size and mtime are unchanged since the last run are skipped; changed files
// are re-parsed in full (global dedupe keys make that idempotent).
func (ing *Ingester) Run(st *store.Store) (Stats, error) {
	var stats Stats
	root, err := ing.root()
	if err != nil {
		return stats, err
	}
	files, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		return stats, err
	}
	sort.Strings(files)

	sourceID, err := st.UpsertSource(SourceKind, root, ing.regime())
	if err != nil {
		return stats, err
	}

	sessionsSeen := map[string]bool{}
	for _, path := range files {
		fi, err := os.Stat(path)
		if err != nil {
			return stats, fmt.Errorf("stat %s: %w", path, err)
		}
		size, mtime := fi.Size(), fi.ModTime().UnixNano()
		if !ing.Full {
			unchanged, err := st.FileUnchanged(sourceID, path, size, mtime)
			if err != nil {
				return stats, err
			}
			if unchanged {
				stats.FilesSkipped++
				continue
			}
		}
		stats.Files++
		if err := ing.ingestFile(st, sourceID, path, &stats, sessionsSeen); err != nil {
			return stats, fmt.Errorf("ingest %s: %w", path, err)
		}
		if err := st.RecordFile(sourceID, path, size, mtime); err != nil {
			return stats, err
		}
	}
	stats.Sessions = len(sessionsSeen)
	return stats, nil
}

func (ing *Ingester) ingestFile(st *store.Store, sourceID int64, path string, stats *Stats, sessionsSeen map[string]bool) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()

	fc := &fileCtx{
		// Fallback session key when a line lacks sessionId: the filename
		// stem (transcript files are named <session-uuid>.jsonl).
		fileStem: strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		sessions: map[string]*sessAgg{},
		toolUses: map[string]toolMeta{},
		stats:    stats,
	}

	// Tool results can be multi-megabyte lines; bufio.Scanner's default
	// limit is far too small, so read with an unbounded ReadBytes loop.
	r := bufio.NewReaderSize(fh, 1<<20)
	for {
		raw, readErr := r.ReadBytes('\n')
		if b := bytes.TrimSpace(raw); len(b) > 0 {
			stats.Lines++
			fc.handleLine(b)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	return st.WithTx(func(tx *store.Tx) error {
		ids := make(map[string]int64, len(fc.sessions))
		for key, sa := range fc.sessions {
			project := ""
			if sa.cwd != "" {
				project = filepath.Base(sa.cwd)
			}
			id, err := tx.UpsertSession(store.Session{
				SourceID:  sourceID,
				HarnessID: key,
				Slug:      sa.slug,
				Project:   project,
				CWD:       sa.cwd,
				Title:     sa.title(),
				Agent:     sa.agent,
				StartedAt: sa.minTS,
				EndedAt:   sa.maxTS,
			})
			if err != nil {
				return fmt.Errorf("upsert session %s: %w", key, err)
			}
			ids[key] = id
			sessionsSeen[key] = true
		}
		for _, pr := range fc.requests {
			pr.req.SessionID = ids[pr.sessionKey]
			inserted, err := tx.InsertRequest(pr.req)
			if err != nil {
				return fmt.Errorf("insert request %s: %w", pr.req.DedupeKey, err)
			}
			if inserted {
				stats.Requests++
			} else {
				stats.Duplicates++
			}
		}
		for _, pb := range fc.blocks {
			pb.blk.SessionID = ids[pb.sessionKey]
			inserted, err := tx.UpsertBlock(pb.blk)
			if err != nil {
				return fmt.Errorf("upsert block %s: %w", pb.blk.DedupeKey, err)
			}
			if inserted {
				stats.Blocks++
			} else {
				stats.BlocksUpdated++
			}
		}
		for _, pc := range fc.compactions {
			pc.comp.SessionID = ids[pc.sessionKey]
			if err := tx.InsertCompaction(pc.comp); err != nil {
				return err
			}
		}
		return nil
	})
}

func (fc *fileCtx) handleLine(b []byte) {
	var ln line
	if err := json.Unmarshal(b, &ln); err != nil {
		fc.stats.ParseErrors++
		return
	}
	key := ln.SessionID
	if key == "" {
		key = fc.fileStem
	}
	sa := fc.session(key)
	sa.observe(&ln)

	switch ln.Type {
	case "assistant":
		fc.handleAssistant(key, &ln)

	case "user":
		fc.handleUser(key, &ln)

	case "system":
		if ln.Subtype == "compact_boundary" && ln.CompactMeta != nil {
			pre := ln.CompactMeta.PreTokens
			fc.compactions = append(fc.compactions, pendingCompaction{
				sessionKey: key,
				comp: store.Compaction{
					TS:        ln.Timestamp,
					Cause:     ln.CompactMeta.Trigger,
					PreTokens: &pre,
				},
			})
			fc.stats.Compactions++
		}

	case "ai-title":
		if ln.AITitle != "" {
			sa.aiTitle = ln.AITitle
		}
	case "custom-title":
		if ln.CustomTitle != "" {
			sa.customTitle = ln.CustomTitle
		}
	case "agent-name":
		if ln.AgentName != "" {
			sa.agent = ln.AgentName
		}
	}
}

func (fc *fileCtx) handleAssistant(key string, ln *line) {
	var m assistantMessage
	if len(ln.Message) == 0 || json.Unmarshal(ln.Message, &m) != nil {
		fc.stats.ParseErrors++
		return
	}
	// Synthetic messages are harness-injected, not API calls: no request
	// row, no blocks (their text never cost output tokens).
	if m.Model == "<synthetic>" {
		fc.stats.Synthetic++
		return
	}
	reqKey := dedupeKey(m.ID, ln.RequestID, ln.UUID)
	if m.Usage != nil {
		req := store.Request{
			TS:                  ln.Timestamp,
			Model:               m.Model,
			Provider:            provider,
			InputTokens:         m.Usage.InputTokens,
			OutputTokens:        m.Usage.OutputTokens,
			CacheCreationTokens: m.Usage.CacheCreationInputTokens,
			CacheReadTokens:     m.Usage.CacheReadInputTokens,
			FinishReason:        m.StopReason,
			Speed:               m.Usage.Speed,
			ServiceTier:         m.Usage.ServiceTier,
			DedupeKey:           reqKey,
			HarnessMsgID:        m.ID,
			HarnessRequestID:    ln.RequestID,
		}
		if cc := m.Usage.CacheCreation; cc != nil {
			five, hour := cc.Ephemeral5m, cc.Ephemeral1h
			req.CacheCreation5m = &five
			req.CacheCreation1h = &hour
		}
		fc.requests = append(fc.requests, pendingRequest{sessionKey: key, req: req})
	}

	var cbs []contentBlock
	if len(m.Content) == 0 || json.Unmarshal(m.Content, &cbs) != nil {
		return
	}
	for _, cb := range cbs {
		switch cb.Type {
		case "text":
			data := []byte(cb.Text)
			fc.addBlock(key, store.Block{
				TS: ln.Timestamp, Kind: "assistant_text", RequestKey: reqKey,
				ByteLen: int64(len(data)), ContentHash: estimate.Hash(data),
				DedupeKey: "cc:ab:" + m.ID + ":" + ln.RequestID + ":t:" + estimate.Hash(data),
			})
		case "thinking":
			data := []byte(cb.Thinking)
			fc.addBlock(key, store.Block{
				TS: ln.Timestamp, Kind: "thinking", RequestKey: reqKey,
				ByteLen: int64(len(data)), ContentHash: estimate.Hash(data),
				DedupeKey: "cc:ab:" + m.ID + ":" + ln.RequestID + ":th:" + estimate.Hash(data),
			})
		case "tool_use":
			var ti toolInput
			json.Unmarshal(cb.Input, &ti) // best-effort; not all inputs have paths
			meta := toolMeta{tool: cb.Name, mcp: mcpServer(cb.Name), file: ti.path()}
			if cb.ID != "" {
				fc.toolUses[cb.ID] = meta
			}
			dk := "cc:tu:" + cb.ID
			if cb.ID == "" {
				dk = "cc:tu:" + reqKey + ":" + estimate.Hash(cb.Input)
			}
			fc.addBlock(key, store.Block{
				TS: ln.Timestamp, Kind: "tool_use", RequestKey: reqKey,
				Tool: meta.tool, MCPServer: meta.mcp, FilePath: meta.file,
				ToolUseID: cb.ID, ByteLen: int64(len(cb.Input)),
				ContentHash: estimate.Hash(cb.Input), DedupeKey: dk,
			})
		}
	}
}

func (fc *fileCtx) handleUser(key string, ln *line) {
	if len(ln.Message) == 0 {
		return
	}
	var um userMessage
	if json.Unmarshal(ln.Message, &um) != nil {
		fc.stats.ParseErrors++
		return
	}

	textKind := "user_text"
	if ln.IsMeta {
		// Harness-injected user content: hook output, system reminders,
		// command transcripts — context the human never typed.
		textKind = "meta_text"
	}
	addText := func(text string) {
		data := []byte(text)
		ns := ln.PromptID
		if ns == "" {
			// promptId is stable across the file copies that resume/fork
			// produce; sessionKey is the fallback namespace. Identical
			// text within one namespace collapses — acceptable, since
			// duplicated lines SHOULD collapse.
			ns = key
		}
		fc.addBlock(key, store.Block{
			TS: ln.Timestamp, Kind: textKind,
			ByteLen: int64(len(data)), ContentHash: estimate.Hash(data),
			DedupeKey: "cc:ut:" + ns + ":" + estimate.Hash(data),
		})
	}

	// content is either a bare string or a list of blocks.
	var s string
	if json.Unmarshal(um.Content, &s) == nil {
		addText(s)
		return
	}
	var cbs []contentBlock
	if json.Unmarshal(um.Content, &cbs) != nil {
		return
	}
	for _, cb := range cbs {
		switch cb.Type {
		case "text":
			addText(cb.Text)
		case "image":
			// Pasted/attached images: bytes/4 on base64 wildly overestimates
			// vision tokens, so images get their own kind and are excluded
			// from text-token calibration... except they ARE input tokens.
			// Kept separate so reports can call them out.
			fc.addBlock(key, store.Block{
				TS: ln.Timestamp, Kind: "image",
				ByteLen: int64(len(cb.Content)), ContentHash: estimate.Hash(cb.Content),
				DedupeKey: "cc:im:" + key + ":" + estimate.Hash(cb.Content),
			})
		case "tool_result":
			meta := fc.toolUses[cb.ToolUseID]
			if meta.file == "" && len(ln.ToolUseResult) > 0 {
				var tur toolUseResultMeta
				if json.Unmarshal(ln.ToolUseResult, &tur) == nil {
					if tur.FilePath != "" {
						meta.file = tur.FilePath
					} else if tur.File != nil {
						meta.file = tur.File.FilePath
					}
				}
			}
			dk := "cc:tr:" + cb.ToolUseID
			if cb.ToolUseID == "" {
				dk = "cc:tr:" + key + ":" + estimate.Hash(cb.Content)
			}
			fc.addBlock(key, store.Block{
				TS: ln.Timestamp, Kind: "tool_result",
				Tool: meta.tool, MCPServer: meta.mcp, FilePath: meta.file,
				ToolUseID: cb.ToolUseID, ByteLen: int64(len(cb.Content)),
				ContentHash: estimate.Hash(cb.Content), IsError: cb.IsError,
				DedupeKey: dk,
			})
		}
	}
}

// dedupeKey builds the globally-unique identity of one API call. The
// (message.id, requestId) pair is Anthropic-assigned and stable across the
// duplicate lines and duplicate files that transcripts produce; the line
// UUID is the fallback for lines missing either.
func dedupeKey(msgID, reqID, uuid string) string {
	if msgID != "" && reqID != "" {
		return "cc:" + msgID + ":" + reqID
	}
	return "cc:uuid:" + uuid
}
