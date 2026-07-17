// Package opencode ingests OpenCode session storage
// (~/.local/share/opencode/storage/) into the store.
//
// Layout, verified empirically (2026-07, OpenCode ~1.1.x; see
// docs/phase1-profiler.md):
//
//	storage/session/<projectDirID>/<ses_*>.json   session metadata
//	storage/message/<ses_*>/<msg_*>.json          one file per message
//	storage/part/...                              message content (not read in M1)
//
// Assistant message files carry role, providerID/modelID, cost, finish, and
// tokens {input, output, reasoning, cache {read, write}}. Unlike Claude
// Code's append-only transcript lines, these files MUTATE IN PLACE while a
// response streams — tokens land when the message completes — so ingestion
// upserts by message id (last write wins) instead of first-write-wins.
//
// OpenCode's own `cost` field is recorded as reported, but it is known to be
// wrong for custom providers (anomalyco/opencode#17223); tokenator computes
// its own costs from tokens later.
package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/erewhon/tokenator/internal/estimate"
	"github.com/erewhon/tokenator/internal/store"
)

const SourceKind = "opencode"

type Ingester struct {
	// Root is the storage directory; empty means
	// ~/.local/share/opencode/storage (respecting XDG_DATA_HOME).
	Root string
	// Regime is the billing regime recorded on the source row. OpenCode
	// mixes providers (local, per-token remote) in one store, so the
	// default is "mixed"; per-provider regime mapping is a later feature.
	Regime string
	// Full re-parses every file regardless of recorded size/mtime.
	Full bool
}

type Stats struct {
	SessionFiles  int // session files parsed (changed since last run)
	MessageFiles  int // message files parsed (changed since last run)
	PartFiles     int // part files parsed (changed since last run)
	FilesSkipped  int
	Sessions      int
	Requests      int // new request rows
	Updated       int // existing rows refreshed (message file mutated)
	Incomplete    int // zero-token, not-yet-completed messages skipped
	Blocks        int
	BlocksUpdated int
	ParseErrors   int
}

func (st Stats) String() string {
	return fmt.Sprintf(
		"session_files=%d message_files=%d part_files=%d (skipped %d unchanged) sessions=%d requests=+%d updated=%d incomplete=%d blocks=+%d/%d parse_errors=%d",
		st.SessionFiles, st.MessageFiles, st.PartFiles, st.FilesSkipped, st.Sessions,
		st.Requests, st.Updated, st.Incomplete, st.Blocks, st.BlocksUpdated, st.ParseErrors)
}

type sessionFile struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	ParentID  string `json:"parentID"` // subagent sessions, when present
	Directory string `json:"directory"`
	Title     string `json:"title"`
	Time      struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

type messageFile struct {
	ID         string  `json:"id"`
	SessionID  string  `json:"sessionID"`
	Role       string  `json:"role"`
	ModelID    string  `json:"modelID"`
	ProviderID string  `json:"providerID"`
	Cost       float64 `json:"cost"`
	Finish     string  `json:"finish"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens *struct {
		Total     int64 `json:"total"`
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// partFile is one content part (storage/part/<msgID>/prt_*.json).
type partFile struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"` // text | reasoning | tool | step-start | step-finish | patch
	Text      string `json:"text"`
	Tool      string `json:"tool"`
	CallID    string `json:"callID"`
	State     *struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output json.RawMessage `json:"output"`
	} `json:"state"`
}

type partToolInput struct {
	FilePath    string `json:"filePath"`
	FilePathAlt string `json:"file_path"`
	Path        string `json:"path"`
}

func (pi partToolInput) path() string {
	for _, p := range []string{pi.FilePath, pi.FilePathAlt, pi.Path} {
		if p != "" {
			return p
		}
	}
	return ""
}

func (ing *Ingester) root() (string, error) {
	if ing.Root != "" {
		return ing.Root, nil
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "opencode", "storage"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode", "storage"), nil
}

func (ing *Ingester) regime() string {
	if ing.Regime != "" {
		return ing.Regime
	}
	return "mixed"
}

// Run ingests changed session and message files. Both kinds are tracked in
// ingest_file; message files are re-parsed whenever size or mtime moves,
// and UpsertRequest makes that refresh idempotent.
func (ing *Ingester) Run(st *store.Store) (Stats, error) {
	var stats Stats
	root, err := ing.root()
	if err != nil {
		return stats, err
	}

	sourceID, err := st.UpsertSource(SourceKind, root, ing.regime())
	if err != nil {
		return stats, err
	}
	states, err := st.FileStates(sourceID)
	if err != nil {
		return stats, err
	}

	changed := func(path string) (int64, int64, bool, error) {
		fi, err := os.Stat(path)
		if err != nil {
			return 0, 0, false, err
		}
		size, mtime := fi.Size(), fi.ModTime().UnixNano()
		if ing.Full {
			return size, mtime, true, nil
		}
		prev, ok := states[path]
		return size, mtime, !ok || prev.Size != size || prev.MtimeNS != mtime, nil
	}

	// Session metadata, keyed by session id, from changed session files.
	sessMeta := map[string]*sessionFile{}
	sessionPaths, err := filepath.Glob(filepath.Join(root, "session", "*", "*.json"))
	if err != nil {
		return stats, err
	}
	sort.Strings(sessionPaths)
	type fileRecord struct {
		path  string
		size  int64
		mtime int64
	}
	var parsedFiles []fileRecord
	for _, path := range sessionPaths {
		size, mtime, isChanged, err := changed(path)
		if err != nil {
			return stats, err
		}
		if !isChanged {
			stats.FilesSkipped++
			continue
		}
		var sf sessionFile
		if err := readJSON(path, &sf); err != nil || sf.ID == "" {
			stats.ParseErrors++
			continue
		}
		stats.SessionFiles++
		sessMeta[sf.ID] = &sf
		parsedFiles = append(parsedFiles, fileRecord{path, size, mtime})
	}

	// Changed message files, grouped by session (dir name = session id).
	messagePaths, err := filepath.Glob(filepath.Join(root, "message", "*", "*.json"))
	if err != nil {
		return stats, err
	}
	sort.Strings(messagePaths)
	bySession := map[string][]*messageFile{}
	for _, path := range messagePaths {
		size, mtime, isChanged, err := changed(path)
		if err != nil {
			return stats, err
		}
		if !isChanged {
			stats.FilesSkipped++
			continue
		}
		var mf messageFile
		if err := readJSON(path, &mf); err != nil || mf.ID == "" {
			stats.ParseErrors++
			continue
		}
		stats.MessageFiles++
		key := mf.SessionID
		if key == "" {
			key = filepath.Base(filepath.Dir(path))
		}
		bySession[key] = append(bySession[key], &mf)
		parsedFiles = append(parsedFiles, fileRecord{path, size, mtime})
	}

	// Changed part files → blocks. Parts carry no role or timestamp of
	// their own, so each looks up its parent message file (cached; parsed
	// messages from this run pre-fill the cache).
	msgCache := map[string]*messageFile{}
	for _, msgs := range bySession {
		for _, mf := range msgs {
			msgCache[mf.ID] = mf
		}
	}
	lookupMsg := func(sesID, msgID string) *messageFile {
		if mf, ok := msgCache[msgID]; ok {
			return mf
		}
		var mf messageFile
		if err := readJSON(filepath.Join(root, "message", sesID, msgID+".json"), &mf); err != nil {
			msgCache[msgID] = nil
			return nil
		}
		msgCache[msgID] = &mf
		return &mf
	}

	type pendingBlock struct {
		sessionKey string
		blk        store.Block
	}
	var pendingBlocks []pendingBlock
	partPaths, err := filepath.Glob(filepath.Join(root, "part", "*", "*.json"))
	if err != nil {
		return stats, err
	}
	sort.Strings(partPaths)
	for _, path := range partPaths {
		size, mtime, isChanged, err := changed(path)
		if err != nil {
			return stats, err
		}
		if !isChanged {
			stats.FilesSkipped++
			continue
		}
		var pf partFile
		if err := readJSON(path, &pf); err != nil || pf.ID == "" {
			stats.ParseErrors++
			continue
		}
		stats.PartFiles++
		parsedFiles = append(parsedFiles, fileRecord{path, size, mtime})
		sesID := pf.SessionID
		msg := lookupMsg(sesID, pf.MessageID)
		role, ts := "", ""
		if msg != nil {
			role = msg.Role
			if sesID == "" {
				sesID = msg.SessionID
			}
			ts = msToRFC3339(msg.Time.Created)
		}
		if sesID == "" {
			continue
		}
		if blk, ok := blockFromPart(&pf, role, ts); ok {
			pendingBlocks = append(pendingBlocks, pendingBlock{sessionKey: sesID, blk: blk})
		}
	}

	// Every session that has new metadata, messages, or parts gets one
	// upsert; sessions only present via messages/parts get a shell row
	// (metadata merges in whenever the session file next changes).
	touched := map[string]bool{}
	for id := range sessMeta {
		touched[id] = true
	}
	for id := range bySession {
		touched[id] = true
	}
	for _, pb := range pendingBlocks {
		touched[pb.sessionKey] = true
	}
	err = st.WithTx(func(tx *store.Tx) error {
		ids := map[string]int64{}
		keys := make([]string, 0, len(touched))
		for id := range touched {
			keys = append(keys, id)
		}
		sort.Strings(keys)
		for _, key := range keys {
			sess := store.Session{SourceID: sourceID, HarnessID: key}
			if sf := sessMeta[key]; sf != nil {
				sess.Slug = sf.Slug
				sess.CWD = sf.Directory
				if sf.Directory != "" {
					sess.Project = filepath.Base(sf.Directory)
				}
				sess.Title = sf.Title
				sess.StartedAt = msToRFC3339(sf.Time.Created)
				sess.EndedAt = msToRFC3339(sf.Time.Updated)
			}
			id, err := tx.UpsertSession(sess)
			if err != nil {
				return fmt.Errorf("upsert session %s: %w", key, err)
			}
			ids[key] = id
		}
		for _, key := range keys {
			for _, mf := range bySession[key] {
				if mf.Role != "assistant" || mf.Tokens == nil {
					continue
				}
				if mf.Time.Completed == 0 && mf.Tokens.Total == 0 {
					stats.Incomplete++
					continue
				}
				ts := mf.Time.Completed
				if ts == 0 {
					ts = mf.Time.Created
				}
				reasoning, cost := mf.Tokens.Reasoning, mf.Cost
				inserted, err := tx.UpsertRequest(store.Request{
					SessionID:           ids[key],
					TS:                  msToRFC3339(ts),
					Model:               mf.ModelID,
					Provider:            mf.ProviderID,
					InputTokens:         mf.Tokens.Input,
					OutputTokens:        mf.Tokens.Output,
					CacheCreationTokens: mf.Tokens.Cache.Write,
					CacheReadTokens:     mf.Tokens.Cache.Read,
					ReasoningTokens:     &reasoning,
					CostUSD:             &cost,
					FinishReason:        mf.Finish,
					DedupeKey:           "oc:" + mf.ID,
					HarnessMsgID:        mf.ID,
				})
				if err != nil {
					return fmt.Errorf("upsert request %s: %w", mf.ID, err)
				}
				if inserted {
					stats.Requests++
				} else {
					stats.Updated++
				}
			}
		}
		for _, pb := range pendingBlocks {
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
		return nil
	})
	if err != nil {
		return stats, err
	}

	for _, fr := range parsedFiles {
		if err := st.RecordFile(sourceID, fr.path, fr.size, fr.mtime); err != nil {
			return stats, err
		}
	}
	stats.Sessions = len(touched)
	return stats, nil
}

// blockFromPart maps an OpenCode part to a block row. Reasoning parts map
// to "thinking"; tool parts become a single tool_result block sized by the
// output (the model-generated input is separate and small); step-start,
// step-finish, and patch parts are bookkeeping, not context.
func blockFromPart(pf *partFile, role, ts string) (store.Block, bool) {
	blk := store.Block{TS: ts, DedupeKey: "oc:pb:" + pf.ID}
	switch pf.Type {
	case "text":
		blk.Kind = "user_text"
		if role == "assistant" {
			blk.Kind = "assistant_text"
		}
		data := []byte(pf.Text)
		blk.ByteLen = int64(len(data))
		blk.ContentHash = estimate.Hash(data)
	case "reasoning":
		data := []byte(pf.Text)
		blk.Kind = "thinking"
		blk.ByteLen = int64(len(data))
		blk.ContentHash = estimate.Hash(data)
	case "tool":
		blk.Kind = "tool_result"
		blk.Tool = pf.Tool
		blk.ToolUseID = "oc:" + pf.CallID
		if pf.State != nil {
			var pi partToolInput
			json.Unmarshal(pf.State.Input, &pi)
			blk.FilePath = pi.path()
			blk.ByteLen = int64(len(pf.State.Output))
			blk.ContentHash = estimate.Hash(pf.State.Output)
			blk.IsError = pf.State.Status == "error"
		}
	default:
		return blk, false
	}
	if blk.ByteLen == 0 {
		return blk, false
	}
	blk.EstTokens = estimate.Tokens(int(blk.ByteLen))
	return blk, true
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// msToRFC3339 renders epoch milliseconds in the same millisecond-precision
// UTC format Claude Code transcripts use, so timestamps sort consistently
// across sources.
func msToRFC3339(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000") + "Z"
}
