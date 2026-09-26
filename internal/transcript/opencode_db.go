package transcript

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// OpenCode 1.18.x stores sessions in SQLite instead of the JSON tree. A
// source whose root is that database file (rather than the storage/
// directory) is read through these helpers; the JSON in message.data and
// part.data is the same shape the tree files had, minus the ids, which are
// columns.

// isOpenCodeDB reports whether an opencode source root is the database
// file rather than the storage directory.
func isOpenCodeDB(root string) bool {
	fi, err := os.Stat(root)
	return err == nil && !fi.IsDir()
}

func openOpenCodeDB(path string) (*sql.DB, error) {
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

// locateOpenCodeDB confirms the session exists in the database; the "file"
// is the database itself.
func locateOpenCodeDB(path, sessionKey string) ([]string, error) {
	db, err := openOpenCodeDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session WHERE id = ?`, sessionKey).Scan(&n); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("no session %s in %s", sessionKey, path)
	}
	return []string{path}, nil
}

// loadOpenCodeDBSession is loadOpenCodeSession over the database: messages
// in id order (k-sortable → chronological), each message's parts likewise.
func loadOpenCodeDBSession(path, sessionKey string) ([]Entry, error) {
	db, err := openOpenCodeDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`
		SELECT m.id, m.data, p.data
		FROM message m LEFT JOIN part p ON p.message_id = m.id
		WHERE m.session_id = ?
		ORDER BY m.id, p.id`, sessionKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	msgs := map[string]*ocMessage{}
	for rows.Next() {
		var id string
		var mdata []byte
		var pdata sql.NullString
		if err := rows.Scan(&id, &mdata, &pdata); err != nil {
			return nil, err
		}
		m, ok := msgs[id]
		if !ok {
			m = &ocMessage{}
			if err := json.Unmarshal(mdata, m); err != nil {
				continue
			}
			m.ID = id
			msgs[id] = m
		}
		if !pdata.Valid {
			continue
		}
		var p ocPart
		if err := json.Unmarshal([]byte(pdata.String), &p); err != nil {
			continue
		}
		ts := ""
		if m.Time.Created > 0 {
			ts = time.UnixMilli(m.Time.Created).UTC().Format(time.RFC3339)
		}
		out = append(out, entriesFromOCPart(&p, m.Role, ts)...)
	}
	return out, rows.Err()
}

// rawMatchOpenCodeDB is the cheap pre-filter: does any part of the session
// contain the (lower-cased) needle? SQLite's LOWER is ASCII-only, matching
// fileContains' behaviour on the tree.
func rawMatchOpenCodeDB(path, sessionKey string, needle []byte) (bool, error) {
	db, err := openOpenCodeDB(path)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM part WHERE session_id = ? AND instr(lower(data), ?) > 0`,
		sessionKey, strings.ToLower(string(needle))).Scan(&n)
	return n > 0, err
}
