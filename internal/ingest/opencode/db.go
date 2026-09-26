package opencode

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/erewhon/tokenator/internal/store"
)

// OpenCode 1.18.x keeps sessions in SQLite (opencode.db). The tables that
// matter, verified against a live database on 2026-09-26:
//
//	session(id, project_id, parent_id, slug, directory, title, time_created, time_updated, …)
//	message(id, session_id, time_created, time_updated, data)
//	part(id, message_id, session_id, time_created, time_updated, data)
//
// `data` is the same JSON the tree layout stored per file, minus the ids and
// sessionID/messageID (those are columns now). Messages still mutate in
// place while a response streams, so a row's time_updated plays the role a
// file's mtime played: ingest_file tracks "<table>/<ids>" keys with
// size=length(data) and mtime_ns=time_updated*1e6, and only rows whose
// pair moved are re-read. The key/length pass never reads `data`, so a
// steady-state run touches the database once per table.

// openRO opens an OpenCode database read-only. The file is WAL-mode and
// may be open in OpenCode at the same time; sqlite handles that.
func openRO(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

func (ing *Ingester) runDB(st *store.Store, path string) (Stats, error) {
	var stats Stats
	db, err := openRO(path)
	if err != nil {
		return stats, err
	}
	defer db.Close()

	sourceID, err := st.UpsertSource(SourceKind, path, ing.regime())
	if err != nil {
		return stats, err
	}
	states, err := st.FileStates(sourceID)
	if err != nil {
		return stats, err
	}
	changed := func(key string, size, updatedMS int64) (int64, bool) {
		mtime := updatedMS * 1_000_000
		if ing.Full {
			return mtime, true
		}
		prev, ok := states[key]
		return mtime, !ok || prev.Size != size || prev.MtimeNS != mtime
	}
	var parsedFiles []fileRecord

	// Sessions: metadata is all columns, no JSON to parse.
	sessMeta := map[string]*sessionFile{}
	rows, err := db.Query(`SELECT id, COALESCE(parent_id, ''), slug, directory, title, time_created, time_updated FROM session`)
	if err != nil {
		return stats, fmt.Errorf("session: %w", err)
	}
	for rows.Next() {
		var sf sessionFile
		if err := rows.Scan(&sf.ID, &sf.ParentID, &sf.Slug, &sf.Directory, &sf.Title, &sf.Time.Created, &sf.Time.Updated); err != nil {
			rows.Close()
			return stats, err
		}
		key := "session/" + sf.ID
		mtime, isChanged := changed(key, 0, sf.Time.Updated)
		if !isChanged {
			stats.FilesSkipped++
			continue
		}
		stats.SessionFiles++
		sessMeta[sf.ID] = &sf
		parsedFiles = append(parsedFiles, fileRecord{key, 0, mtime})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return stats, err
	}

	// Messages: keys and lengths first, data only for the changed rows.
	type rowKey struct {
		id, parent, session string
		size, updated       int64
	}
	scanKeys := func(q string) ([]rowKey, error) {
		rows, err := db.Query(q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []rowKey
		for rows.Next() {
			var k rowKey
			if err := rows.Scan(&k.id, &k.parent, &k.session, &k.size, &k.updated); err != nil {
				return nil, err
			}
			out = append(out, k)
		}
		return out, rows.Err()
	}
	msgKeys, err := scanKeys(`SELECT id, '', session_id, length(data), time_updated FROM message ORDER BY id`)
	if err != nil {
		return stats, fmt.Errorf("message: %w", err)
	}
	bySession := map[string][]*messageFile{}
	msgCache := map[string]*messageFile{}
	loadMessage := func(id, session string) (*messageFile, error) {
		var data []byte
		if err := db.QueryRow(`SELECT data FROM message WHERE id = ?`, id).Scan(&data); err != nil {
			return nil, err
		}
		var mf messageFile
		if err := json.Unmarshal(data, &mf); err != nil {
			return nil, err
		}
		mf.ID, mf.SessionID = id, session // columns, not in data
		return &mf, nil
	}
	for _, k := range msgKeys {
		key := "message/" + k.session + "/" + k.id
		mtime, isChanged := changed(key, k.size, k.updated)
		if !isChanged {
			stats.FilesSkipped++
			continue
		}
		mf, err := loadMessage(k.id, k.session)
		if err != nil {
			stats.ParseErrors++
			continue
		}
		stats.MessageFiles++
		bySession[k.session] = append(bySession[k.session], mf)
		msgCache[k.id] = mf
		parsedFiles = append(parsedFiles, fileRecord{key, k.size, mtime})
	}

	// Parts → blocks. Role and timestamp come from the parent message,
	// which may be unchanged this run (then it is read on demand).
	lookupMsg := func(id, session string) *messageFile {
		if mf, ok := msgCache[id]; ok {
			return mf
		}
		mf, err := loadMessage(id, session)
		if err != nil {
			mf = nil
		}
		msgCache[id] = mf
		return mf
	}
	partKeys, err := scanKeys(`SELECT id, message_id, session_id, length(data), time_updated FROM part ORDER BY id`)
	if err != nil {
		return stats, fmt.Errorf("part: %w", err)
	}
	var pendingBlocks []pendingBlock
	for _, k := range partKeys {
		key := "part/" + k.parent + "/" + k.id
		mtime, isChanged := changed(key, k.size, k.updated)
		if !isChanged {
			stats.FilesSkipped++
			continue
		}
		var data []byte
		if err := db.QueryRow(`SELECT data FROM part WHERE id = ?`, k.id).Scan(&data); err != nil {
			stats.ParseErrors++
			continue
		}
		var pf partFile
		if err := json.Unmarshal(data, &pf); err != nil {
			stats.ParseErrors++
			continue
		}
		pf.ID, pf.MessageID, pf.SessionID = k.id, k.parent, k.session
		stats.PartFiles++
		parsedFiles = append(parsedFiles, fileRecord{key, k.size, mtime})
		role, ts := "", ""
		if msg := lookupMsg(k.parent, k.session); msg != nil {
			role, ts = msg.Role, msToRFC3339(msg.Time.Created)
		}
		if blk, ok := blockFromPart(&pf, role, ts); ok {
			pendingBlocks = append(pendingBlocks, pendingBlock{sessionKey: k.session, blk: blk})
		}
	}
	// Deterministic order for the commit (the tree reader sorts paths).
	sort.SliceStable(pendingBlocks, func(i, j int) bool { return pendingBlocks[i].blk.DedupeKey < pendingBlocks[j].blk.DedupeKey })
	return commit(st, sourceID, sessMeta, bySession, pendingBlocks, parsedFiles, stats)
}
