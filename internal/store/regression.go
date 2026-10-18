package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Regression runs (docs/decisions/2026-10-18-regression-trigger.md): the approver queues one
// (Slack's `@jarvis-loop regression [web|mobile|all]`, the board's button), a runner that asks for
// kind regression takes it, runs jarvis-server's scripts/e2e, uploads the failures' screenshots
// and reports the tests; 'File it' turns a failure into an item, once per test while it is open.

// Run statuses.
const (
	RegQueued  = "queued"
	RegRunning = "running"
	RegPassed  = "passed"
	RegFailed  = "failed" // a test failed, or a suite did not run
	RegError   = "error"  // the run produced no report
)

// Event kinds of the runs (no item).
const (
	KindRegQueued  = "regression_queued"
	KindRegClaimed = "regression_claimed"
	KindRegDone    = "regression_done"
	KindRegExpired = "regression_expired"
	KindRegStopped = "regression_stopped"
)

// CodeRegressionOpen refuses a second run while one is queued or running.
const CodeRegressionOpen = "regression_open"

// Limits on what a run reports.
const (
	MaxRegressionTests = 2000
	MaxRegressionFiles = 60
)

// Regression is one regression run.
type Regression struct {
	ID          int64      `json:"id"`
	Suite       string     `json:"suite"`
	Status      string     `json:"status"`
	RequestedBy string     `json:"requested_by"`
	Via         string     `json:"via,omitempty"`
	Runner      string     `json:"runner,omitempty"`
	Agent       string     `json:"agent,omitempty"`
	Error       string     `json:"error,omitempty"`
	E2ERun      string     `json:"e2e_run,omitempty"`
	ServerHead  string     `json:"server_head,omitempty"`
	MobileHead  string     `json:"mobile_head,omitempty"`
	Passed      int        `json:"passed"`
	Failed      int        `json:"failed"`
	Skipped     int        `json:"skipped"`
	ClaimedAt   *time.Time `json:"claimed_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// Tests and Files are a finished run's (GetRegression only).
	Tests []RegressionTest `json:"tests,omitempty"`
	Files []RegressionFile `json:"files,omitempty"`

	lease Lease
}

// Resources are what its suite needs (pipeline.SuiteResources).
func (r Regression) Resources() []string { return pipeline.SuiteResources(r.Suite) }

// RegressionTest is one test of a finished run.
type RegressionTest struct {
	N      int    `json:"n"`
	Suite  string `json:"suite"`
	Title  string `json:"title"`
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	MS     int64  `json:"ms"`
	// Screenshot is the regression file n of its screenshot (0: none).
	Screenshot int    `json:"screenshot,omitempty"`
	Repo       string `json:"repo,omitempty"`
	// ItemID is the open item 'File it' made for this test (by any run), 0 when none.
	ItemID int64 `json:"item_id,omitempty"`
}

// Failed says the test failed (scripts/e2e's failed, timedOut and interrupted).
func (t RegressionTest) Failed() bool { return t.Status != "passed" && t.Status != "skipped" }

// Key is what 'File it' deduplicates on.
func (t RegressionTest) Key() string { return t.Suite + ": " + t.Title }

// RegressionFile is a file a run uploaded.
type RegressionFile struct {
	N           int       `json:"n"`
	Name        string    `json:"name"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	At          time.Time `json:"at"`
	path        string
}

// RegressionReport is a runner finishing the run it holds: the tests of scripts/e2e's
// report.json, or why there is no report (Error; with tests, why a suite did not run).
type RegressionReport struct {
	Error      string                 `json:"error"`
	E2ERun     string                 `json:"e2e_run"`
	ServerHead string                 `json:"server_head"`
	MobileHead string                 `json:"mobile_head"`
	Tests      []RegressionReportTest `json:"tests"`
}

// RegressionReportTest is one test as the runner reports it; Screenshot names a file it uploaded.
type RegressionReportTest struct {
	Suite      string `json:"suite"`
	Title      string `json:"title"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Status     string `json:"status"`
	Error      string `json:"error"`
	MS         int64  `json:"ms"`
	Screenshot string `json:"screenshot"`
	Repo       string `json:"repo"`
}

const regCols = `id, suite, status, requested_by, via, runner, agent, lease_id, claimed_at, expires_at, note, activity,
	error, e2e_run, server_head, mobile_head, passed, failed, skipped, finished_at, created_at, updated_at`

func scanRegression(row scanner) (Regression, error) {
	var (
		r                                   Regression
		act                                 string
		claimed, expires, finished, cr, upd int64
	)
	err := row.Scan(&r.ID, &r.Suite, &r.Status, &r.RequestedBy, &r.Via, &r.Runner, &r.Agent, &r.lease.ID, &claimed, &expires,
		&r.lease.Note, &act, &r.Error, &r.E2ERun, &r.ServerHead, &r.MobileHead, &r.Passed, &r.Failed, &r.Skipped, &finished, &cr, &upd)
	if err != nil {
		return r, err
	}
	r.CreatedAt, r.UpdatedAt = fromNanos(cr), fromNanos(upd)
	if claimed != 0 {
		c := fromNanos(claimed)
		r.ClaimedAt = &c
		r.lease.ClaimedAt, r.lease.ExpiresAt = c, fromNanos(expires)
	}
	if finished != 0 {
		f := fromNanos(finished)
		r.FinishedAt = &f
	}
	r.lease.Stage, r.lease.Runner, r.lease.Agent, r.lease.Resources, r.lease.Activity =
		pipeline.RegressionStage, r.Runner, r.Agent, r.Resources(), parseActivity(act)
	return r, nil
}

func saveRegression(tx *sql.Tx, r Regression) error {
	var claimed, expires, finished int64
	if r.ClaimedAt != nil {
		claimed = r.ClaimedAt.UnixNano()
	}
	if !r.lease.ExpiresAt.IsZero() {
		expires = r.lease.ExpiresAt.UnixNano()
	}
	if r.FinishedAt != nil {
		finished = r.FinishedAt.UnixNano()
	}
	_, err := tx.Exec(`UPDATE regressions SET status=?, runner=?, agent=?, lease_id=?, claimed_at=?, expires_at=?, note=?, activity=?,
		error=?, e2e_run=?, server_head=?, mobile_head=?, passed=?, failed=?, skipped=?, finished_at=?, updated_at=? WHERE id = ?`,
		r.Status, r.Runner, r.Agent, r.lease.ID, claimed, expires, r.lease.Note, encodedActivity(r.lease.Activity),
		r.Error, r.E2ERun, r.ServerHead, r.MobileHead, r.Passed, r.Failed, r.Skipped, finished, r.UpdatedAt.UnixNano(), r.ID)
	return err
}

// openRegression is the queued or running run, or nil (there is at most one).
func openRegression(q querier) (*Regression, error) {
	r, err := scanRegression(q.QueryRow(`SELECT `+regCols+` FROM regressions WHERE status IN (?, ?) ORDER BY id LIMIT 1`, RegQueued, RegRunning))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func regEventData(r Regression) map[string]any {
	return map[string]any{"regression": r.ID, "suite": r.Suite}
}

// QueueRegression queues a run of suite for actor (the approver; the callers check), via slack,
// board or api. While one is queued or running, a second is refused (CodeRegressionOpen).
func (s *Store) QueueRegression(ctx context.Context, actor, suite, via string) (Regression, error) {
	var out Regression
	suite = strings.ToLower(strings.TrimSpace(suite))
	if suite == "" {
		suite = pipeline.SuiteAll
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if !slices.Contains(pipeline.Suites, suite) {
			return errf(pipeline.CodeInvalidField, "suite must be one of %s, not %q", strings.Join(pipeline.Suites, ", "), suite)
		}
		if err := s.sweep(tx); err != nil {
			return err
		}
		open, err := openRegression(tx)
		if err != nil {
			return err
		}
		if open != nil {
			return errf(CodeRegressionOpen, "regression R%d (%s) is already %s; one runs at a time", open.ID, open.Suite, open.Status)
		}
		now := s.clock.Now()
		res, err := tx.Exec(`INSERT INTO regressions (suite, status, requested_by, via, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
			suite, RegQueued, actor, via, now.UnixNano(), now.UnixNano())
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		out = Regression{ID: id, Suite: suite, Status: RegQueued, RequestedBy: actor, Via: via, CreatedAt: now, UpdatedAt: now}
		data := regEventData(out)
		data["via"] = via
		return s.event(tx, Event{Actor: actor, Kind: KindRegQueued, Data: data})
	})
	if err != nil {
		return out, s.refuse(ctx, actor, 0, "regression", err)
	}
	return out, nil
}

// regCandidate is the queued run a claim may start, or nil.
func regCandidate(tx *sql.Tx) (*pipeline.RegCandidate, error) {
	r, err := openRegression(tx)
	if err != nil || r == nil || r.Status != RegQueued {
		return nil, err
	}
	return &pipeline.RegCandidate{RunID: r.ID, Suite: r.Suite, Resources: r.Resources(), CreatedAt: r.CreatedAt}, nil
}

// claimReg starts the queued run.
func (s *Store) claimReg(tx *sql.Tx, runnerName, agent string, c pipeline.RegCandidate) (*Claim, error) {
	now := s.clock.Now()
	r, err := scanRegression(tx.QueryRow(`SELECT `+regCols+` FROM regressions WHERE id = ?`, c.RunID))
	if err != nil {
		return nil, err
	}
	id, err := newLeaseID()
	if err != nil {
		return nil, err
	}
	r.Status, r.Runner, r.Agent, r.UpdatedAt, r.ClaimedAt = RegRunning, runnerName, agent, now, &now
	r.lease = Lease{ID: id, Stage: pipeline.RegressionStage, Runner: runnerName, Agent: agent, Resources: r.Resources(),
		ClaimedAt: now, ExpiresAt: now.Add(LeaseTTL)}
	if err := saveRegression(tx, r); err != nil {
		return nil, err
	}
	data := regEventData(r)
	data["runner"], data["agent"], data["lease"] = runnerName, agent, id
	if err := s.event(tx, Event{Actor: "runner:" + runnerName, Kind: KindRegClaimed, Data: data}); err != nil {
		return nil, err
	}
	return &Claim{Kind: pipeline.WorkRegression, Stage: pipeline.RegressionStage, Lease: r.lease, Reg: &r}, nil
}

// requeueReg puts a run whose lease ended early back in the queue (its runner went away, or the
// loop was stopped): the user asked for it, and nothing of it was reported.
func (s *Store) requeueReg(tx *sql.Tx, r Regression) error {
	r.Status, r.Runner, r.Agent, r.ClaimedAt, r.lease, r.UpdatedAt = RegQueued, "", "", nil, Lease{}, s.clock.Now()
	return saveRegression(tx, r)
}

// sweepReg ends the running run's lease when it expired (the run waits again) or ran past
// pipeline.RegressionMaxRun (it ends with an error).
func (s *Store) sweepReg(tx *sql.Tx, now time.Time) error {
	r, err := openRegression(tx)
	if err != nil || r == nil || r.Status != RegRunning {
		return err
	}
	data := regEventData(*r)
	data["lease"], data["runner"], data["agent"], data["note"] = r.lease.ID, r.Runner, r.Agent, r.lease.Note
	switch pipeline.JudgeLease(r.lease.ClaimedAt, r.lease.ExpiresAt, now, pipeline.RegressionMaxRun) {
	case pipeline.LeaseExpired:
		if err := s.requeueReg(tx, *r); err != nil {
			return err
		}
		return s.event(tx, Event{Actor: "loopd", Kind: KindRegExpired, Data: data})
	case pipeline.LeaseOvertime:
		r.Status, r.Error, r.UpdatedAt, r.FinishedAt = RegError, fmt.Sprintf("the run went past %s", pipeline.RegressionMaxRun), now, &now
		r.lease.ID = ""
		if err := saveRegression(tx, *r); err != nil {
			return err
		}
		data["status"], data["error"] = r.Status, r.Error
		return s.event(tx, Event{Actor: "loopd", Kind: KindRegDone, Data: data})
	}
	return nil
}

// regHeartbeat is Heartbeat for the running run's lease. found is false when no run holds it.
func (s *Store) regHeartbeat(tx *sql.Tx, runnerName, leaseID, note, actJSON string) (stop, found bool, err error) {
	r, err := scanRegression(tx.QueryRow(`SELECT `+regCols+` FROM regressions WHERE lease_id = ?`, leaseID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, true, err
	}
	if r.Runner != runnerName {
		return false, true, errf(CodeNotYourLease, "lease %s is held by another runner", leaseID)
	}
	now := s.clock.Now()
	if _, err := tx.Exec(`UPDATE runners SET last_seen = ? WHERE name = ?`, now.UnixNano(), runnerName); err != nil {
		return false, true, err
	}
	if r.Status != RegRunning {
		return true, true, nil
	}
	ctl, err := s.loadControl(tx)
	if err != nil {
		return false, true, err
	}
	if ctl.Stopped {
		data := regEventData(r)
		data["lease"], data["runner"], data["agent"], data["note"] = leaseID, r.Runner, r.Agent, note
		if err := s.requeueReg(tx, r); err != nil {
			return true, true, err
		}
		return true, true, s.event(tx, Event{Actor: "loopd", Kind: KindRegStopped, Data: data})
	}
	if note != "" {
		r.lease.Note = note
	}
	if actJSON != "" {
		r.lease.Activity = parseActivity(actJSON)
	}
	r.lease.ExpiresAt = now.Add(LeaseTTL)
	return false, true, saveRegression(tx, r)
}

// heldRegression is the run runnerName holds the live lease of.
func heldRegression(tx *sql.Tx, id int64, runnerName string) (Regression, error) {
	r, err := scanRegression(tx.QueryRow(`SELECT `+regCols+` FROM regressions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, errf(CodeNotFound, "no regression R%d", id)
	}
	if err != nil {
		return r, err
	}
	if r.Status != RegRunning {
		return r, errf(CodeNoLease, "regression R%d has no live lease (it is %s)", id, r.Status)
	}
	if r.Runner != runnerName {
		return r, errf(CodeNotYourLease, "regression R%d is leased by another runner", id)
	}
	return r, nil
}

// AddRegressionFile stores a file of the run runnerName holds (a failure's screenshot, the
// report), at most MaxEvidenceBytes each and MaxRegressionFiles a run.
func (s *Store) AddRegressionFile(ctx context.Context, runnerName string, id int64, name string, body io.Reader) (RegressionFile, error) {
	var f RegressionFile
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx); err != nil {
			return err
		}
		_, err := heldRegression(tx, id, runnerName)
		return err
	})
	if err != nil {
		return f, err
	}
	dir := filepath.Join(s.dir, "regressions", fmt.Sprint(id))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return f, err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return f, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	h := sha256.New()
	head := &sniffer{}
	n, err := io.Copy(io.MultiWriter(tmp, h, head), io.LimitReader(body, MaxEvidenceBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return f, err
	}
	if n > MaxEvidenceBytes {
		return f, errf(CodeTooLarge, "a regression file is limited to %d bytes", MaxEvidenceBytes)
	}
	clean := SanitizeName(name)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx); err != nil {
			return err
		}
		if _, err := heldRegression(tx, id, runnerName); err != nil {
			return err
		}
		var next int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(n), 0) + 1 FROM regression_files WHERE run_id = ?`, id).Scan(&next); err != nil {
			return err
		}
		if next > MaxRegressionFiles {
			return errf(CodeTooLarge, "at most %d files a run", MaxRegressionFiles)
		}
		rel := filepath.Join("regressions", fmt.Sprint(id), fmt.Sprintf("%d-%s", next, clean))
		f = RegressionFile{N: next, Name: clean, SHA256: hex.EncodeToString(h.Sum(nil)), Size: n,
			ContentType: http.DetectContentType(head.b), At: s.clock.Now(), path: rel}
		if _, err := tx.Exec(`INSERT INTO regression_files (run_id, n, name, path, sha256, size, content_type, at) VALUES (?,?,?,?,?,?,?,?)`,
			id, f.N, f.Name, rel, f.SHA256, f.Size, f.ContentType, f.At.UnixNano()); err != nil {
			return err
		}
		return os.Rename(tmpName, filepath.Join(s.dir, rel))
	})
	if err != nil {
		if f.path != "" {
			os.Remove(filepath.Join(s.dir, f.path))
		}
		return RegressionFile{}, err
	}
	return f, nil
}

// RegressionResult finishes the run runnerName holds: its tests (and their screenshots, by the
// name they were uploaded under), or why it could not run.
func (s *Store) RegressionResult(ctx context.Context, runnerName string, id int64, rep RegressionReport) (Regression, error) {
	var out Regression
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx); err != nil {
			return err
		}
		r, err := heldRegression(tx, id, runnerName)
		if err != nil {
			return err
		}
		if len(rep.Tests) > MaxRegressionTests {
			return errf(pipeline.CodeInvalidField, "at most %d tests, got %d", MaxRegressionTests, len(rep.Tests))
		}
		files, err := regressionFiles(tx, id)
		if err != nil {
			return err
		}
		byName := map[string]int{}
		for _, f := range files {
			byName[f.Name] = f.N
		}
		now := s.clock.Now()
		r.Passed, r.Failed, r.Skipped = 0, 0, 0
		for i, t := range rep.Tests {
			if strings.TrimSpace(t.Title) == "" || strings.TrimSpace(t.Status) == "" {
				return errf(pipeline.CodeMissingField, "test %d has no title or status", i+1)
			}
			rt := RegressionTest{N: i + 1, Suite: clipText(t.Suite, 40), Title: clipText(t.Title, 500), File: clipText(t.File, 300),
				Line: t.Line, Status: clipText(t.Status, 40), Error: clipText(t.Error, 4000), MS: max(t.MS, 0), Repo: clipText(t.Repo, 200)}
			if t.Screenshot != "" {
				rt.Screenshot = byName[SanitizeName(t.Screenshot)]
			}
			switch {
			case rt.Status == "passed":
				r.Passed++
			case rt.Status == "skipped":
				r.Skipped++
			default:
				r.Failed++
			}
			if _, err := tx.Exec(`INSERT INTO regression_tests (run_id, n, suite, title, file, line, status, error, ms, screenshot, repo)
				VALUES (?,?,?,?,?,?,?,?,?,?,?)`, id, rt.N, rt.Suite, rt.Title, rt.File, rt.Line, rt.Status, rt.Error, rt.MS, rt.Screenshot, rt.Repo); err != nil {
				return err
			}
		}
		r.Error = clipText(rep.Error, 2000)
		r.E2ERun, r.ServerHead, r.MobileHead = clipText(rep.E2ERun, 80), clipText(rep.ServerHead, 64), clipText(rep.MobileHead, 64)
		switch {
		case len(rep.Tests) == 0:
			r.Status = RegError
			if r.Error == "" {
				r.Error = "the run reported no tests"
			}
		case r.Failed > 0 || r.Error != "":
			r.Status = RegFailed
		default:
			r.Status = RegPassed
		}
		r.UpdatedAt, r.FinishedAt = now, &now
		leaseID := r.lease.ID
		r.lease = Lease{}
		if err := saveRegression(tx, r); err != nil {
			return err
		}
		out = r
		data := regEventData(r)
		data["lease"], data["status"], data["passed"], data["failed"], data["skipped"] = leaseID, r.Status, r.Passed, r.Failed, r.Skipped
		if r.Error != "" {
			data["error"] = r.Error
		}
		return s.event(tx, Event{Actor: "runner:" + runnerName, Kind: KindRegDone, Data: data})
	})
	return out, err
}

func regressionFiles(q querier, id int64) ([]RegressionFile, error) {
	rows, err := q.Query(`SELECT n, name, path, sha256, size, content_type, at FROM regression_files WHERE run_id = ? ORDER BY n`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RegressionFile{}
	for rows.Next() {
		var f RegressionFile
		var at int64
		if err := rows.Scan(&f.N, &f.Name, &f.path, &f.SHA256, &f.Size, &f.ContentType, &at); err != nil {
			return nil, err
		}
		f.At = fromNanos(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

func regressionTests(q querier, id int64) ([]RegressionTest, error) {
	rows, err := q.Query(`SELECT t.n, t.suite, t.title, t.file, t.line, t.status, t.error, t.ms, t.screenshot, t.repo,
		COALESCE((SELECT ri.item_id FROM regression_items ri JOIN items i ON i.id = ri.item_id
			WHERE ri.test = t.suite || ': ' || t.title AND i.stage NOT IN ('done', 'closed') ORDER BY ri.item_id DESC LIMIT 1), 0)
		FROM regression_tests t WHERE t.run_id = ? ORDER BY t.n`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RegressionTest{}
	for rows.Next() {
		var t RegressionTest
		if err := rows.Scan(&t.N, &t.Suite, &t.Title, &t.File, &t.Line, &t.Status, &t.Error, &t.MS, &t.Screenshot, &t.Repo, &t.ItemID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Regressions are the runs, newest first (at most limit), without their tests.
func (s *Store) Regressions(ctx context.Context, limit int) ([]Regression, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+regCols+` FROM regressions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Regression{}
	for rows.Next() {
		r, err := scanRegression(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRegression is a run with its tests and files.
func (s *Store) GetRegression(ctx context.Context, id int64) (Regression, error) {
	r, err := scanRegression(s.r.QueryRowContext(ctx, `SELECT `+regCols+` FROM regressions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, errf(CodeNotFound, "no regression R%d", id)
	}
	if err != nil {
		return r, err
	}
	if r.Tests, err = regressionTests(s.r, id); err != nil {
		return r, err
	}
	r.Files, err = regressionFiles(s.r, id)
	return r, err
}

// OpenRegressionFile opens file n of run id. The caller closes the file.
func (s *Store) OpenRegressionFile(ctx context.Context, id int64, n int) (RegressionFile, *os.File, error) {
	var f RegressionFile
	var at int64
	err := s.r.QueryRowContext(ctx, `SELECT n, name, path, sha256, size, content_type, at FROM regression_files WHERE run_id = ? AND n = ?`,
		id, n).Scan(&f.N, &f.Name, &f.path, &f.SHA256, &f.Size, &f.ContentType, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return f, nil, errf(CodeNotFound, "no file %d on regression R%d", n, id)
	}
	if err != nil {
		return f, nil, err
	}
	f.At = fromNanos(at)
	file, err := os.Open(filepath.Join(s.dir, f.path))
	return f, file, err
}

// FileRegressionFailure is 'File it' on test n of run id: a new item in the test's repo with the
// failure (and its screenshot as evidence), or the open item already filed for the same test
// (existing true). Only a failed test can be filed.
func (s *Store) FileRegressionFailure(ctx context.Context, actor string, id int64, n int) (it pipeline.Item, existing bool, err error) {
	r, err := s.GetRegression(ctx, id)
	if err != nil {
		return it, false, err
	}
	var t *RegressionTest
	for i := range r.Tests {
		if r.Tests[i].N == n {
			t = &r.Tests[i]
		}
	}
	if t == nil {
		return it, false, errf(CodeNotFound, "regression R%d has no test %d", id, n)
	}
	if !t.Failed() {
		return it, false, errf(pipeline.CodeInvalidField, "test %d of R%d %s; only a failure is filed", n, id, t.Status)
	}
	repo, err := s.ResolveRepo(t.Repo)
	if err != nil {
		repo = "" // triage picks it
	}
	var shot *RegressionFile
	for i := range r.Files {
		if r.Files[i].N == t.Screenshot {
			shot = &r.Files[i]
		}
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var open int64
		err := tx.QueryRow(`SELECT ri.item_id FROM regression_items ri JOIN items i ON i.id = ri.item_id
			WHERE ri.test = ? AND i.stage NOT IN ('done', 'closed') ORDER BY ri.item_id DESC LIMIT 1`, t.Key()).Scan(&open)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if open != 0 {
			existing = true
			it, err = loadItem(tx, open)
			return err
		}
		it, err = pipeline.NewItem(clipText("E2E failing: "+t.Title, 200), regressionAsk(r, *t), repo, "P2", "finding", s.clock.Now())
		if err != nil {
			return err
		}
		if err := s.insertItem(tx, actor, &it); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO regression_items (item_id, test, run_id, n) VALUES (?,?,?,?)`, it.ID, t.Key(), id, n); err != nil {
			return err
		}
		if shot == nil {
			return nil
		}
		return s.attachRegressionFile(tx, actor, it.ID, *shot, "screenshot: "+clipText(t.Title, 150))
	})
	return it, existing, err
}

// attachRegressionFile copies a run's file into an item's evidence (no lease: loopd files it).
func (s *Store) attachRegressionFile(tx *sql.Tx, actor string, itemID int64, f RegressionFile, label string) error {
	src, err := os.Open(filepath.Join(s.dir, f.path))
	if err != nil {
		return err
	}
	defer src.Close()
	dir := filepath.Join(s.dir, "evidence", fmt.Sprint(itemID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	rel := filepath.Join("evidence", fmt.Sprint(itemID), "1-"+f.Name)
	dst, err := os.OpenFile(filepath.Join(s.dir, rel), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	at := s.clock.Now()
	if _, err := tx.Exec(`INSERT INTO evidence (item_id, n, lease_id, label, name, path, sha256, size, content_type, uploaded_by, at)
		VALUES (?,1,'',?,?,?,?,?,?,?,?)`, itemID, label, f.Name, rel, f.SHA256, f.Size, f.ContentType, actor, at.UnixNano()); err != nil {
		os.Remove(filepath.Join(s.dir, rel))
		return err
	}
	return s.event(tx, Event{ItemID: itemID, Actor: actor, Kind: "evidence",
		Data: map[string]any{"n": 1, "name": f.Name, "label": label, "size": f.Size, "sha256": f.SHA256}})
}

func regressionAsk(r Regression, t RegressionTest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The end-to-end regression R%d (%s", r.ID, r.Suite)
	if r.ServerHead != "" {
		fmt.Fprintf(&b, ", jarvis-server %s", short12(r.ServerHead))
	}
	if r.MobileHead != "" {
		fmt.Fprintf(&b, ", jarvis-node-mobile %s", short12(r.MobileHead))
	}
	fmt.Fprintf(&b, ") failed this test:\n\n%s: %s", t.Suite, t.Title)
	if t.File != "" {
		fmt.Fprintf(&b, " (%s:%d)", t.File, t.Line)
	}
	fmt.Fprintf(&b, ", %s\n", t.Status)
	if t.Error != "" {
		fmt.Fprintf(&b, "\n```\n%s\n```\n", clipText(t.Error, 3000))
	}
	fmt.Fprintf(&b, "\nThe run's report is on the board: /board/regressions/%d", r.ID)
	if t.Screenshot != 0 {
		b.WriteString(" (its screenshot is this item's evidence)")
	}
	b.WriteString(".\nFind why it fails and fix it (the app, or the test when the test is wrong); `scripts/e2e " + t.Suite +
		" --filter '<its title>'` runs it alone.")
	return b.String()
}

func short12(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
