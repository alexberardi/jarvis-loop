package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// The overnight regression (docs/decisions/2026-10-19-overnight-regression.md): a run of suite
// overnight is scheduled (status scheduled, at most one) by the approver, the board's toggle or the
// control's regression_nightly; the claim path keeps since when the queue was quiet and starts it
// once it has been for pipeline.OvernightQuiet past its set time; its report waits for the morning
// (report_at), which the Slack bot's tick posts.

// scheduledRegression is the scheduled run, or nil.
func scheduledRegression(q querier) (*Regression, error) {
	r, err := scanRegression(q.QueryRow(`SELECT `+regCols+` FROM regressions WHERE status = ? LIMIT 1`, RegScheduled))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ScheduledRegression is the run scheduled for tonight, or nil.
func (s *Store) ScheduledRegression(ctx context.Context) (*Regression, error) {
	return scheduledRegression(s.r)
}

// ScheduleRegression schedules the overnight run for actor (the approver; the callers check), via
// slack, board or api: at the control's regression_at (the next one) or once the queue is quiet,
// whichever is later. One is scheduled at a time (CodeRegressionOpen).
func (s *Store) ScheduleRegression(ctx context.Context, actor, via string) (Regression, error) {
	var out Regression
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx); err != nil {
			return err
		}
		ctl, err := s.loadControl(tx)
		if err != nil {
			return err
		}
		out, err = s.schedule(tx, actor, via, ctl)
		return err
	})
	if err != nil {
		return out, s.refuse(ctx, actor, 0, "regression_schedule", err)
	}
	return out, nil
}

func (s *Store) schedule(tx *sql.Tx, actor, via string, ctl pipeline.Control) (Regression, error) {
	var out Regression
	sched, err := scheduledRegression(tx)
	if err != nil {
		return out, err
	}
	if sched != nil {
		return out, errf(CodeRegressionOpen, "regression R%d (overnight) is already scheduled for %s", sched.ID, s.clockText(*sched.NotBefore))
	}
	now := s.clock.Now()
	at := pipeline.NextAt(now, s.orchLoc(), ctl.RegressionTime())
	if via == "nightly" {
		// Once a night, keyed on the set time: a night that already has an overnight row (any
		// status: run, running, or cancelled to skip it) moves to the next. The 12 hours only absorb
		// a changed regression_at and the clock change; set times are a day apart.
		for {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM regressions WHERE suite = ? AND not_before > ? AND not_before <= ?`,
				pipeline.SuiteOvernight, at.Add(-12*time.Hour).UnixNano(), at.UnixNano()).Scan(&n); err != nil {
				return out, err
			}
			if n == 0 {
				break
			}
			at = pipeline.NextAt(at.Add(time.Minute), s.orchLoc(), ctl.RegressionTime())
		}
	}
	res, err := tx.Exec(`INSERT INTO regressions (suite, status, requested_by, via, not_before, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
		pipeline.SuiteOvernight, RegScheduled, actor, via, at.UnixNano(), now.UnixNano(), now.UnixNano())
	if err != nil {
		return out, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return out, err
	}
	out = Regression{ID: id, Suite: pipeline.SuiteOvernight, Status: RegScheduled, RequestedBy: actor, Via: via, NotBefore: &at,
		CreatedAt: now, UpdatedAt: now}
	data := regEventData(out)
	data["via"], data["not_before"] = via, at.Format(time.RFC3339)
	return out, s.event(tx, Event{Actor: actor, Kind: KindRegScheduled, Data: data})
}

// clockText is t on the loop's clock, as the messages say it ("01:00 on Tue Oct 20").
func (s *Store) clockText(t time.Time) string {
	return t.In(s.orchLoc()).Format("15:04 on Mon Jan 2")
}

// CancelScheduledRegression cancels the scheduled run (CodeNotFound when there is none).
func (s *Store) CancelScheduledRegression(ctx context.Context, actor, via string) (Regression, error) {
	var out Regression
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := scheduledRegression(tx)
		if err != nil {
			return err
		}
		if r == nil {
			return errf(CodeNotFound, "no regression is scheduled")
		}
		out, err = s.cancelScheduled(tx, actor, via, *r)
		return err
	})
	if err != nil {
		return out, s.refuse(ctx, actor, 0, "regression_cancel", err)
	}
	return out, nil
}

func (s *Store) cancelScheduled(tx *sql.Tx, actor, via string, r Regression) (Regression, error) {
	now := s.clock.Now()
	r.Status, r.UpdatedAt, r.FinishedAt = RegCancelled, now, &now
	if err := saveRegression(tx, r); err != nil {
		return r, err
	}
	data := regEventData(r)
	data["via"] = via
	return r, s.event(tx, Event{Actor: actor, Kind: KindRegCancelled, Data: data})
}

// overnight is the claim path's part (Claim, every runner's poll): it schedules tonight's run when
// the control's regression_nightly is on and none is (and cancels the nightly one when it was
// turned off), keeps since when the queue was quiet on the scheduled run, and gives it as the
// candidate when it may start (no other run open, OvernightReady).
func (s *Store) overnight(tx *sql.Tx, cs claimState, turns []pipeline.TurnCandidate, now time.Time) (*pipeline.RegCandidate, error) {
	sched, err := scheduledRegression(tx)
	if err != nil {
		return nil, err
	}
	switch {
	case sched == nil && cs.ctl.RegressionNightly:
		r, err := s.schedule(tx, "loopd", "nightly", cs.ctl)
		if err != nil {
			return nil, err
		}
		sched = &r
	case sched != nil && sched.Via == "nightly" && !cs.ctl.RegressionNightly:
		_, err := s.cancelScheduled(tx, "loopd", "nightly", *sched)
		return nil, err
	}
	if sched == nil {
		return nil, nil
	}
	orch, err := s.orchCandidate(tx, cs.ctl)
	if err != nil {
		return nil, err
	}
	busy := pipeline.QueueBusy(cs.ctl, cs.pace, cs.waiting, cs.deps, cs.live, turns, orch != nil, cs.frozen, cs.runners, now) != ""
	switch {
	case busy && sched.QuietSince != nil:
		sched.QuietSince = nil
	case !busy && sched.QuietSince == nil:
		sched.QuietSince = &now
	default:
		return s.overnightCandidate(tx, *sched, now)
	}
	sched.UpdatedAt = now
	if err := saveRegression(tx, *sched); err != nil {
		return nil, err
	}
	return s.overnightCandidate(tx, *sched, now)
}

func (s *Store) overnightCandidate(tx *sql.Tx, r Regression, now time.Time) (*pipeline.RegCandidate, error) {
	var quiet time.Time
	if r.QuietSince != nil {
		quiet = *r.QuietSince
	}
	if !pipeline.OvernightReady(*r.NotBefore, quiet, now) {
		return nil, nil
	}
	open, err := openRegression(tx)
	if err != nil || open != nil {
		return nil, err
	}
	return &pipeline.RegCandidate{RunID: r.ID, Suite: r.Suite, Resources: r.Resources(), CreatedAt: r.CreatedAt}, nil
}

// setReportAt sets when a finished overnight run is reported: the morning (pipeline.MorningReport).
// Other runs are reported as they end.
func (s *Store) setReportAt(r *Regression, now time.Time) {
	if !r.Overnight() {
		return
	}
	at := pipeline.MorningReport(now, s.orchLoc())
	r.ReportAt = &at
}

// DueReport is a finished overnight run whose report is due, with what its regression_done event
// said (the QA column's items it verified or reopened).
type DueReport struct {
	Run  Regression
	Done map[string]any
}

// DueRegressionReports are the finished overnight runs whose report is due and not yet posted.
func (s *Store) DueRegressionReports(ctx context.Context) ([]DueReport, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT id FROM regressions WHERE report_at != 0 AND report_at <= ? AND reported_at = 0 ORDER BY id`,
		s.clock.Now().UnixNano())
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []DueReport
	for _, id := range ids {
		r, err := s.GetRegression(ctx, id)
		if err != nil {
			return nil, err
		}
		d := DueReport{Run: r, Done: map[string]any{}}
		var raw string
		err = s.r.QueryRowContext(ctx, `SELECT data FROM events WHERE kind = ? AND json_extract(data, '$.regression') = ? ORDER BY id DESC LIMIT 1`,
			KindRegDone, id).Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &d.Done)
		}
		out = append(out, d)
	}
	return out, nil
}

// MarkRegressionReported records that a run's report was posted.
func (s *Store) MarkRegressionReported(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE regressions SET reported_at = ? WHERE id = ? AND reported_at = 0`, s.clock.Now().UnixNano(), id)
		return err
	})
}

// UnstartedRun is a scheduled overnight run still not started at pipeline.UnstartedNoteAt after its
// set time, and why it waits (#317).
type UnstartedRun struct {
	Run Regression
	Why string
}

// UnstartedOvernight is the scheduled overnight run whose note is due and not yet posted
// (MarkUnstartedNoted), or nil: its set time has passed and so has the next UnstartedNoteAt. Why
// says what keeps it: the queue's work (pipeline.QueueBusy), claude's pace, another open run, or
// a quiet queue no runner has polled since.
func (s *Store) UnstartedOvernight(ctx context.Context) (*UnstartedRun, error) {
	var out *UnstartedRun
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sched, err := scheduledRegression(tx)
		if err != nil || sched == nil || sched.NotBefore == nil {
			return err
		}
		now := s.clock.Now()
		if now.Before(*sched.NotBefore) || now.Before(pipeline.NextAt(*sched.NotBefore, s.orchLoc(), pipeline.UnstartedNoteAt)) {
			return nil
		}
		var noted int64
		if err := tx.QueryRow(`SELECT unstarted_noted_at FROM regressions WHERE id = ?`, sched.ID).Scan(&noted); err != nil || noted != 0 {
			return err
		}
		cs, err := s.claimState(tx, now)
		if err != nil {
			return err
		}
		turns, err := turnCandidates(tx)
		if err != nil {
			return err
		}
		orch, err := s.orchCandidate(tx, cs.ctl)
		if err != nil {
			return err
		}
		open, err := openRegression(tx)
		if err != nil {
			return err
		}
		var why []string
		switch busy := pipeline.QueueBusy(cs.ctl, cs.pace, cs.waiting, cs.deps, cs.live, turns, orch != nil, cs.frozen, cs.runners, now); {
		case busy != "":
			why = append(why, "the queue is busy: "+busy)
		case open != nil:
			why = append(why, fmt.Sprintf("R%d (%s) is still open", open.ID, open.Suite))
		case sched.QuietSince != nil:
			why = append(why, "the queue has been quiet since "+s.clockText(*sched.QuietSince)+"; it starts after "+pipeline.OvernightQuiet.String()+" of quiet")
		default:
			why = append(why, "the queue is quiet, but no runner has asked for work since it went quiet")
		}
		if cs.pace.Level != pipeline.PaceNormal {
			why = append(why, "claude's pace: "+cs.pace.Describe(usageZone()))
		}
		out = &UnstartedRun{Run: *sched, Why: strings.Join(why, "; ")}
		return nil
	})
	return out, err
}

// MarkUnstartedNoted records that the unstarted run's note was posted.
func (s *Store) MarkUnstartedNoted(ctx context.Context, id int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE regressions SET unstarted_noted_at = ? WHERE id = ? AND unstarted_noted_at = 0`, s.clock.Now().UnixNano(), id)
		return err
	})
}
