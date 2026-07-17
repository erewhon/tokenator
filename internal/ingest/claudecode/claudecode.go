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
}

type Stats struct {
	Files        int
	FilesSkipped int
	Lines        int
	ParseErrors  int
	Sessions     int
	Requests     int
	Duplicates   int
	Synthetic    int
	Compactions  int
}

func (st Stats) String() string {
	return fmt.Sprintf(
		"files=%d (skipped %d unchanged) lines=%d parse_errors=%d sessions=%d requests=+%d dup=%d synthetic=%d compactions=%d",
		st.Files, st.FilesSkipped, st.Lines, st.ParseErrors, st.Sessions,
		st.Requests, st.Duplicates, st.Synthetic, st.Compactions)
}

// line is the union of the top-level transcript fields we care about.
type line struct {
	Type        string          `json:"type"`
	UUID        string          `json:"uuid"`
	SessionID   string          `json:"sessionId"`
	Timestamp   string          `json:"timestamp"`
	CWD         string          `json:"cwd"`
	Slug        string          `json:"slug"`
	RequestID   string          `json:"requestId"`
	Subtype     string          `json:"subtype"`
	Message     json.RawMessage `json:"message"`
	AITitle     string          `json:"aiTitle"`
	CustomTitle string          `json:"customTitle"`
	AgentName   string          `json:"agentName"`
	CompactMeta *struct {
		Trigger   string `json:"trigger"`
		PreTokens int64  `json:"preTokens"`
	} `json:"compactMetadata"`
}

type assistantMessage struct {
	ID         string `json:"id"`
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Usage      *usage `json:"usage"`
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
		unchanged, err := st.FileUnchanged(sourceID, path, size, mtime)
		if err != nil {
			return stats, err
		}
		if unchanged {
			stats.FilesSkipped++
			continue
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

	// Fallback session key when a line lacks sessionId: the filename stem
	// (transcript files are named <session-uuid>.jsonl).
	fileStem := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	sessions := map[string]*sessAgg{}
	var requests []pendingRequest
	var compactions []pendingCompaction

	session := func(key string) *sessAgg {
		sa := sessions[key]
		if sa == nil {
			sa = &sessAgg{}
			sessions[key] = sa
		}
		return sa
	}

	// Tool results can be multi-megabyte lines; bufio.Scanner's default
	// limit is far too small, so read with an unbounded ReadBytes loop.
	r := bufio.NewReaderSize(fh, 1<<20)
	for {
		raw, readErr := r.ReadBytes('\n')
		if b := bytes.TrimSpace(raw); len(b) > 0 {
			stats.Lines++
			ing.handleLine(b, fileStem, session, stats, &requests, &compactions)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	return st.WithTx(func(tx *store.Tx) error {
		ids := make(map[string]int64, len(sessions))
		for key, sa := range sessions {
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
		for _, pr := range requests {
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
		for _, pc := range compactions {
			pc.comp.SessionID = ids[pc.sessionKey]
			if err := tx.InsertCompaction(pc.comp); err != nil {
				return err
			}
		}
		return nil
	})
}

func (ing *Ingester) handleLine(b []byte, fileStem string, session func(string) *sessAgg,
	stats *Stats, requests *[]pendingRequest, compactions *[]pendingCompaction) {

	var ln line
	if err := json.Unmarshal(b, &ln); err != nil {
		stats.ParseErrors++
		return
	}
	key := ln.SessionID
	if key == "" {
		key = fileStem
	}
	sa := session(key)
	sa.observe(&ln)

	switch ln.Type {
	case "assistant":
		var m assistantMessage
		if len(ln.Message) == 0 || json.Unmarshal(ln.Message, &m) != nil {
			stats.ParseErrors++
			return
		}
		if m.Model == "<synthetic>" {
			stats.Synthetic++
			return
		}
		if m.Usage == nil {
			return
		}
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
			DedupeKey:           dedupeKey(m.ID, ln.RequestID, ln.UUID),
			HarnessMsgID:        m.ID,
			HarnessRequestID:    ln.RequestID,
		}
		if cc := m.Usage.CacheCreation; cc != nil {
			five, hour := cc.Ephemeral5m, cc.Ephemeral1h
			req.CacheCreation5m = &five
			req.CacheCreation1h = &hour
		}
		*requests = append(*requests, pendingRequest{sessionKey: key, req: req})

	case "system":
		if ln.Subtype == "compact_boundary" && ln.CompactMeta != nil {
			pre := ln.CompactMeta.PreTokens
			*compactions = append(*compactions, pendingCompaction{
				sessionKey: key,
				comp: store.Compaction{
					TS:        ln.Timestamp,
					Cause:     ln.CompactMeta.Trigger,
					PreTokens: &pre,
				},
			})
			stats.Compactions++
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
