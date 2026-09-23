package store

// Per-model view for `tokenator serve` (/model/{name}): which sessions used a
// model, and what the router saw for it. A router alias is what a harness
// sends when it talks to the router, so gw_request.model holds it verbatim;
// resolved_via holds the registry id it landed on; request.model holds the
// harness's own name. Matching all three makes one page answer
// `pitf model <alias>` whichever name the caller has.

import "sort"

// ModelSessionRow is one session that used the model.
type ModelSessionRow struct {
	Key      string // harness_session_id
	Title    string
	Project  string
	Agent    string
	LastTS   string
	Requests int64
	Input    int64
	Output   int64
	// Via says where the counts came from: "transcript" (the harness's own
	// request rows named the model) or "router" (only gateway rows did —
	// the harness called it something else).
	Via string
}

// ModelTotals summarizes the router's rows for the model, paired or not.
type ModelTotals struct {
	Requests int64
	Input    int64
	Output   int64
	Unpaired int64 // gateway rows attributed to no session
}

// ModelSessions returns the sessions that used name (exact, case-sensitive
// match on request.model, gw_request.model or gw_request.resolved_via),
// newest first, at most limit (0 = 100), plus router-side totals.
func (s *Store) ModelSessions(name string, limit int) ([]ModelSessionRow, ModelTotals, error) {
	if limit <= 0 {
		limit = 100
	}
	var tot ModelTotals
	type agg struct {
		n, in, out int64
		last       string
		via        string
	}
	bySession := map[int64]*agg{}

	// The harness's view: its own request rows that name the model.
	rows, err := s.db.Query(`
		SELECT session_id, COUNT(*), SUM(input_tokens), SUM(output_tokens), MAX(ts)
		FROM request WHERE model = ? GROUP BY session_id`, name)
	if err != nil {
		return nil, tot, err
	}
	for rows.Next() {
		var id int64
		a := &agg{via: "transcript"}
		if err := rows.Scan(&id, &a.n, &a.in, &a.out, &a.last); err != nil {
			rows.Close()
			return nil, tot, err
		}
		bySession[id] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, tot, err
	}

	// The router's view, attributed to a session by the session id it
	// logged, else by the transcript request it was paired with.
	rows, err = s.db.Query(`
		SELECT COALESCE(
				(SELECT MIN(id) FROM session WHERE g.session_id != '' AND harness_session_id = g.session_id),
				(SELECT r.session_id FROM request r WHERE g.request_key != '' AND r.dedupe_key = g.request_key)),
			COALESCE(g.input_tokens, 0), COALESCE(g.output_tokens, 0), g.ts
		FROM gw_request g
		WHERE g.model = ? OR g.resolved_via = ?`, name, name)
	if err != nil {
		return nil, tot, err
	}
	gw := map[int64]*agg{}
	for rows.Next() {
		var sid *int64
		var in, out int64
		var ts string
		if err := rows.Scan(&sid, &in, &out, &ts); err != nil {
			rows.Close()
			return nil, tot, err
		}
		tot.Requests++
		tot.Input += in
		tot.Output += out
		if sid == nil {
			tot.Unpaired++
			continue
		}
		a := gw[*sid]
		if a == nil {
			a = &agg{via: "router"}
			gw[*sid] = a
		}
		a.n++
		a.in += in
		a.out += out
		if ts > a.last {
			a.last = ts
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, tot, err
	}
	// Transcript counts win where both exist: they are the harness's own
	// record and include requests that never crossed the router.
	for id, a := range gw {
		if _, ok := bySession[id]; !ok {
			bySession[id] = a
		}
	}

	out := make([]ModelSessionRow, 0, len(bySession))
	for id, a := range bySession {
		var r ModelSessionRow
		err := s.db.QueryRow(`
			SELECT harness_session_id, title, project, agent FROM session WHERE id = ?`, id).
			Scan(&r.Key, &r.Title, &r.Project, &r.Agent)
		if err != nil {
			return nil, tot, err
		}
		r.LastTS, r.Requests, r.Input, r.Output, r.Via = a.last, a.n, a.in, a.out, a.via
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastTS != out[j].LastTS {
			return out[i].LastTS > out[j].LastTS
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, tot, nil
}
