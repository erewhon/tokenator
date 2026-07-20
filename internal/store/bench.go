package store

import (
	"database/sql"
	"fmt"
)

// schemaV6 adds the bench harness (`tokenator bench`): experiment runs and
// their trials. Usage numbers are deliberately NOT copied here — each trial
// ingests its isolated transcript root into this same database, and reports
// join request rows through the per-trial source root at read time (which
// also sweeps in subagent/sidechain sessions belonging to the trial).
const schemaV6 = `
CREATE TABLE bench_run (
	id          INTEGER PRIMARY KEY,
	run_key     TEXT NOT NULL UNIQUE,      -- e.g. 20260719-2101-claude-mem-onoff
	name        TEXT NOT NULL DEFAULT '',
	spec_json   TEXT NOT NULL,             -- resolved spec snapshot (resume + provenance)
	run_dir     TEXT NOT NULL DEFAULT '',
	started_at  TEXT NOT NULL DEFAULT '',
	finished_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE bench_trial (
	id                 INTEGER PRIMARY KEY,
	run_id             INTEGER NOT NULL REFERENCES bench_run(id),
	arm                TEXT NOT NULL,
	trial_idx          INTEGER NOT NULL,
	harness            TEXT NOT NULL,               -- claude_code | opencode
	model              TEXT NOT NULL DEFAULT '',
	harness_session_id TEXT NOT NULL DEFAULT '',    -- CC: pre-assigned; OC: parsed from events
	status             TEXT NOT NULL DEFAULT 'pending', -- pending|running|ok|fail|timeout|error
	checks_json        TEXT NOT NULL DEFAULT '[]',
	started_at         TEXT NOT NULL DEFAULT '',
	finished_at        TEXT NOT NULL DEFAULT '',
	duration_ms        INTEGER NOT NULL DEFAULT 0,
	cost_usd           REAL,                        -- harness-reported; NULL when not reported
	workspace          TEXT NOT NULL DEFAULT '',
	transcript_root    TEXT NOT NULL DEFAULT '',
	ingested           INTEGER NOT NULL DEFAULT 0,
	notes              TEXT NOT NULL DEFAULT '',
	UNIQUE (run_id, arm, trial_idx)
);
CREATE INDEX idx_bench_trial_run ON bench_trial(run_id);
CREATE INDEX idx_bench_trial_session ON bench_trial(harness_session_id) WHERE harness_session_id != '';
`

// Bench trial statuses. ok/fail are completed observations (fail = a check
// failed, still a valid data point); timeout/error are rerunnable.
const (
	TrialPending = "pending"
	TrialRunning = "running"
	TrialOK      = "ok"
	TrialFail    = "fail"
	TrialTimeout = "timeout"
	TrialError   = "error"
)

type BenchRun struct {
	ID         int64
	RunKey     string
	Name       string
	SpecJSON   string
	RunDir     string
	StartedAt  string
	FinishedAt string
}

type BenchTrial struct {
	RunID            int64
	Arm              string
	TrialIdx         int
	Harness          string
	Model            string
	HarnessSessionID string
	Status           string
	ChecksJSON       string
	StartedAt        string
	FinishedAt       string
	DurationMS       int64
	CostUSD          *float64
	Workspace        string
	TranscriptRoot   string
	Ingested         bool
	Notes            string
}

func (s *Store) InsertBenchRun(runKey, name, specJSON, runDir, startedAt string) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO bench_run (run_key, name, spec_json, run_dir, started_at)
		VALUES (?, ?, ?, ?, ?)`, runKey, name, specJSON, runDir, startedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishBenchRun(runID int64, finishedAt string) error {
	_, err := s.db.Exec(`UPDATE bench_run SET finished_at = ? WHERE id = ?`, finishedAt, runID)
	return err
}

// UpsertBenchTrial writes a trial row keyed by (run_id, arm, trial_idx).
// Called once when a trial starts (status running) and again when it ends;
// a resume rerun overwrites the previous attempt's row.
func (s *Store) UpsertBenchTrial(t BenchTrial) error {
	if t.ChecksJSON == "" {
		t.ChecksJSON = "[]"
	}
	_, err := s.db.Exec(`
		INSERT INTO bench_trial (run_id, arm, trial_idx, harness, model,
			harness_session_id, status, checks_json, started_at, finished_at,
			duration_ms, cost_usd, workspace, transcript_root, ingested, notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (run_id, arm, trial_idx) DO UPDATE SET
			harness = excluded.harness, model = excluded.model,
			harness_session_id = excluded.harness_session_id,
			status = excluded.status, checks_json = excluded.checks_json,
			started_at = excluded.started_at, finished_at = excluded.finished_at,
			duration_ms = excluded.duration_ms, cost_usd = excluded.cost_usd,
			workspace = excluded.workspace, transcript_root = excluded.transcript_root,
			ingested = excluded.ingested, notes = excluded.notes`,
		t.RunID, t.Arm, t.TrialIdx, t.Harness, t.Model,
		t.HarnessSessionID, t.Status, t.ChecksJSON, t.StartedAt, t.FinishedAt,
		t.DurationMS, t.CostUSD, t.Workspace, t.TranscriptRoot, t.Ingested, t.Notes)
	return err
}

// MarkBenchTrialIngested flips the ingested bit without touching the rest of
// the row (used by `bench ingest` backfills).
func (s *Store) MarkBenchTrialIngested(runID int64, arm string, trialIdx int, ok bool) error {
	_, err := s.db.Exec(`
		UPDATE bench_trial SET ingested = ? WHERE run_id = ? AND arm = ? AND trial_idx = ?`,
		ok, runID, arm, trialIdx)
	return err
}

// BenchRunByPrefix resolves a run-key prefix to one run; empty prefix means
// the most recently started run.
func (s *Store) BenchRunByPrefix(prefix string) (BenchRun, error) {
	var r BenchRun
	err := s.db.QueryRow(`
		SELECT id, run_key, name, spec_json, run_dir, started_at, finished_at
		FROM bench_run
		WHERE ? = '' OR run_key LIKE ? || '%'
		ORDER BY started_at DESC LIMIT 1`, prefix, prefix).Scan(
		&r.ID, &r.RunKey, &r.Name, &r.SpecJSON, &r.RunDir, &r.StartedAt, &r.FinishedAt)
	if err == sql.ErrNoRows {
		return r, fmt.Errorf("no bench run matches %q", prefix)
	}
	return r, err
}

// BenchRunSummary is one row of `bench list`.
type BenchRunSummary struct {
	RunKey     string
	Name       string
	StartedAt  string
	FinishedAt string
	Trials     int64
	Done       int64 // ok + fail
}

func (s *Store) ListBenchRuns(limit int) ([]BenchRunSummary, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
		SELECT r.run_key, r.name, r.started_at, r.finished_at,
			COUNT(t.id), COALESCE(SUM(t.status IN ('ok','fail')), 0)
		FROM bench_run r LEFT JOIN bench_trial t ON t.run_id = r.id
		GROUP BY r.id ORDER BY r.started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BenchRunSummary
	for rows.Next() {
		var r BenchRunSummary
		if err := rows.Scan(&r.RunKey, &r.Name, &r.StartedAt, &r.FinishedAt, &r.Trials, &r.Done); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BenchTrialUsage is one trial with its joined request usage. HasUsage is
// false when the trial's transcript root was never ingested (report renders
// dashes and points at `bench ingest`).
type BenchTrialUsage struct {
	Arm              string
	TrialIdx         int
	Harness          string
	Model            string
	HarnessSessionID string
	Status           string
	ChecksJSON       string
	DurationMS       int64
	CostUSD          *float64 // harness-reported (CC)
	Requests         int64
	HasUsage         bool
	Input            int64
	Output           int64
	CacheRead        int64
	CacheWrite       int64
	ReqCostUSD       float64 // summed request-level cost (OC reports these)
}

// BenchReportRows joins each trial to every request ingested from its
// isolated transcript root — session-uuid-agnostic, so CC subagent
// sidechains land in the right trial automatically.
func (s *Store) BenchReportRows(runID int64) ([]BenchTrialUsage, error) {
	rows, err := s.db.Query(`
		SELECT t.arm, t.trial_idx, t.harness, t.model, t.harness_session_id,
			t.status, t.checks_json, t.duration_ms, t.cost_usd,
			COUNT(r.id), COUNT(r.id) > 0,
			COALESCE(SUM(r.input_tokens), 0), COALESCE(SUM(r.output_tokens), 0),
			COALESCE(SUM(r.cache_read_tokens), 0), COALESCE(SUM(r.cache_creation_tokens), 0),
			COALESCE(SUM(r.cost_usd), 0)
		FROM bench_trial t
		LEFT JOIN source src ON src.kind = t.harness AND src.root = t.transcript_root
		LEFT JOIN session s ON s.source_id = src.id
		LEFT JOIN request r ON r.session_id = s.id
		WHERE t.run_id = ?
		GROUP BY t.id
		ORDER BY t.arm, t.trial_idx`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BenchTrialUsage
	for rows.Next() {
		var u BenchTrialUsage
		if err := rows.Scan(&u.Arm, &u.TrialIdx, &u.Harness, &u.Model, &u.HarnessSessionID,
			&u.Status, &u.ChecksJSON, &u.DurationMS, &u.CostUSD,
			&u.Requests, &u.HasUsage, &u.Input, &u.Output,
			&u.CacheRead, &u.CacheWrite, &u.ReqCostUSD); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// BenchTrials returns the raw trial rows of a run (resume + ingest backfill).
func (s *Store) BenchTrials(runID int64) ([]BenchTrial, error) {
	rows, err := s.db.Query(`
		SELECT run_id, arm, trial_idx, harness, model, harness_session_id,
			status, checks_json, started_at, finished_at, duration_ms,
			cost_usd, workspace, transcript_root, ingested, notes
		FROM bench_trial WHERE run_id = ? ORDER BY arm, trial_idx`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BenchTrial
	for rows.Next() {
		var t BenchTrial
		if err := rows.Scan(&t.RunID, &t.Arm, &t.TrialIdx, &t.Harness, &t.Model,
			&t.HarnessSessionID, &t.Status, &t.ChecksJSON, &t.StartedAt, &t.FinishedAt,
			&t.DurationMS, &t.CostUSD, &t.Workspace, &t.TranscriptRoot, &t.Ingested, &t.Notes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
