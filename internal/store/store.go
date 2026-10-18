// Package store keeps loopd's state in SQLite (modernc.org/sqlite, no cgo) and applies the
// pipeline rules inside its transactions. All writes go through one connection, so the rules see
// a consistent snapshot and two claims can never interleave.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // the database/sql driver "sqlite"

	"github.com/alexberardi/jarvis-loop/internal/clock"
	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

//go:embed migrations/*.sql
var migrations embed.FS

// LeaseTTL is how long a lease lives without a heartbeat.
const LeaseTTL = 5 * time.Minute

// Error codes the store adds to the pipeline's.
const (
	CodeNotFound     = "not_found"
	CodeNoLease      = "no_lease"
	CodeNotYourLease = "not_your_lease"
	CodeTooLarge     = "too_large"
)

func errf(code, format string, args ...any) *pipeline.Error {
	return &pipeline.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Store is loopd's database and evidence directory.
type Store struct {
	dir   string
	w     *sql.DB // the one writer
	r     *sql.DB // readers
	clock clock.Clock

	// wmu serializes write transactions (there is one writer connection anyway) and guards
	// pending, the events written by the open transaction. emitMu hands them to subscribers after
	// commit, in commit order.
	wmu     sync.Mutex
	emitMu  sync.Mutex
	pending []Event
	via     string // the open transaction's WithVia: every event it writes says so
	subsMu  sync.RWMutex
	subs    []func(Event)

	reposMu sync.RWMutex
	repos   []string
	aliases map[string]string   // lower-case alias -> slug (loopd's --repos-file)
	rules   []pipeline.HoldRule // the built-in hold rules; nil: pipeline.DefaultHoldRules
	// loopRepo is the loop's own repo (SetLoopRepo): a merge to it starts an orchestrate turn.
	loopRepo string

	// tickMu guards tickAt, when Sweep last ran OrchTick.
	tickMu sync.Mutex
	tickAt time.Time
}

// SetRepos sets the repo slugs (owner/name) loopd is configured with (loopd's --repos): the only
// repos triage may give an item that has none.
func (s *Store) SetRepos(slugs []string) {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	s.repos = append([]string(nil), slugs...)
}

// Repos are the configured repo slugs.
func (s *Store) Repos() []string {
	s.reposMu.RLock()
	defer s.reposMu.RUnlock()
	return append([]string(nil), s.repos...)
}

// SetAliases sets the repo aliases (alias -> slug, loopd's --repos-file) a new item's repo may
// name.
func (s *Store) SetAliases(aliases map[string]string) {
	s.reposMu.Lock()
	defer s.reposMu.Unlock()
	s.aliases = map[string]string{}
	for a, slug := range aliases {
		s.aliases[normRepo(a)] = slug
	}
}

func normRepo(name string) string { return strings.Join(strings.Fields(strings.ToLower(name)), " ") }

// ResolveRepo gives the configured slug a new item's repo names: the slug itself, an alias, or
// the name part of one configured slug (loop add --repo jarvis-server), case-insensitively. An
// empty repo stays empty (routing picks it), and so does any name while no repos are configured.
// Anything else is refused.
func (s *Store) ResolveRepo(name string) (string, error) {
	s.reposMu.RLock()
	defer s.reposMu.RUnlock()
	n := normRepo(name)
	if n == "" || len(s.repos) == 0 {
		return strings.TrimSpace(name), nil
	}
	for _, slug := range s.repos {
		if strings.ToLower(slug) == n {
			return slug, nil
		}
	}
	if slug, ok := s.aliases[n]; ok {
		return slug, nil
	}
	var found []string
	for _, slug := range s.repos {
		if _, short, ok := strings.Cut(slug, "/"); ok && strings.ToLower(short) == n {
			found = append(found, slug)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", errf(pipeline.CodeInvalidField, "repo %q is not one loopd is configured with (%s)", strings.TrimSpace(name), strings.Join(s.repos, ", "))
	default:
		return "", errf(pipeline.CodeInvalidField, "repo %q names %s: give the owner/name", strings.TrimSpace(name), strings.Join(found, " and "))
	}
}

// Subscribe registers fn to receive every event after the transaction that wrote it commits, in
// commit order. fn runs on the committing goroutine: it must be quick and must not call the
// store's write methods (the Slack layer only queues the event).
func (s *Store) Subscribe(fn func(Event)) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	s.subs = append(s.subs, fn)
}

// Open opens (creating if needed) the data directory and its database, and applies migrations.
func Open(dir string, clk clock.Clock) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "evidence"), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "loop.db")
	pragmas := "_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + pragmas
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	s := &Store{dir: dir, w: w, clock: clk}
	if err := s.migrate(); err != nil {
		w.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := s.resetBuiltInCopy(); err != nil {
		w.Close()
		return nil, fmt.Errorf("hold rules: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn+"&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, err
	}
	s.r = r
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	errR := s.r.Close()
	return errors.Join(s.w.Close(), errR)
}

func (s *Store) migrate() error {
	if _, err := s.w.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	ents, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	for _, e := range ents {
		v, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", e.Name())
		}
		body, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		err = s.tx(context.Background(), func(tx *sql.Tx) error {
			var n int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, v).Scan(&n); err != nil || n > 0 {
				return err
			}
			if _, err := tx.Exec(string(body)); err != nil {
				return fmt.Errorf("%s: %w", e.Name(), err)
			}
			_, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, v, s.clock.Now().UnixNano())
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion is the highest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.w.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	s.wmu.Lock()
	s.pending = nil
	s.via = ViaFrom(ctx)
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		s.wmu.Unlock()
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		s.pending = nil
		s.wmu.Unlock()
		return err
	}
	if err := tx.Commit(); err != nil {
		s.pending = nil
		s.wmu.Unlock()
		return err
	}
	evs := s.pending
	s.pending = nil
	// Take the emit lock before letting the next writer in, so events leave in commit order.
	s.emitMu.Lock()
	s.wmu.Unlock()
	defer s.emitMu.Unlock()
	if len(evs) == 0 {
		return nil
	}
	s.subsMu.RLock()
	subs := append([]func(Event){}, s.subs...)
	s.subsMu.RUnlock()
	for _, e := range evs {
		for _, fn := range subs {
			fn(e)
		}
	}
	return nil
}

// Event is one entry of an item's history (or a control change, with ItemID 0).
type Event struct {
	ID      int64          `json:"id"`
	ItemID  int64          `json:"item_id,omitempty"`
	PlanID  int64          `json:"plan_id,omitempty"`
	At      time.Time      `json:"at"`
	Actor   string         `json:"actor"`
	Kind    string         `json:"kind"`
	From    string         `json:"from,omitempty"`
	To      string         `json:"to,omitempty"`
	Outcome string         `json:"outcome,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

func (s *Store) event(tx *sql.Tx, e Event) error {
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	if s.via != "" {
		e.Data["via"] = s.via
	}
	data, err := json.Marshal(e.Data)
	if err != nil {
		return err
	}
	var item, plan any
	if e.ItemID != 0 {
		item = e.ItemID
	}
	if e.PlanID != 0 {
		plan = e.PlanID
	}
	at := s.clock.Now()
	res, err := tx.Exec(`INSERT INTO events (item_id, plan_id, at, actor, kind, from_stage, to_stage, outcome, data) VALUES (?,?,?,?,?,?,?,?,?)`,
		item, plan, at.UnixNano(), e.Actor, e.Kind, e.From, e.To, e.Outcome, string(data))
	if err != nil {
		return err
	}
	if e.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	if err := s.orchHook(tx, e); err != nil {
		return err
	}
	// Subscribers get the event as a reader would see it (data round-tripped through JSON).
	e.At = fromNanos(at.UnixNano())
	e.Data = map[string]any{}
	if err := json.Unmarshal(data, &e.Data); err != nil {
		return err
	}
	s.pending = append(s.pending, e)
	return nil
}

func transitionEvent(id int64, actor string, tr pipeline.Transition) Event {
	return Event{ItemID: id, Actor: actor, Kind: tr.Kind, From: string(tr.From), To: string(tr.To), Outcome: string(tr.Outcome), Data: tr.Data}
}

// refuse records a refused action on an item, then returns err. Refusals of actions on items that
// do not exist are not recorded (there is no history to add to).
func (s *Store) refuse(ctx context.Context, actor string, id int64, action string, err error) error {
	var pe *pipeline.Error
	if !errors.As(err, &pe) || pe.Code == CodeNotFound {
		return err
	}
	_ = s.tx(ctx, func(tx *sql.Tx) error {
		return s.event(tx, Event{ItemID: id, Actor: actor, Kind: "refused",
			Data: map[string]any{"action": action, "code": pe.Code, "message": pe.Message}})
	})
	return err
}

// ---- items ----

const itemCols = `id, title, ask, repo, source, priority, stage, return_stage, question, area, size, risk,
	resources, bounce_count, pr, branch, head_sha, builder_agent, merge_sha, summary, approval, created_at, updated_at, hold_reason, needs_qa, reopen_of, ci_red_sha, ci_main_fixed,
	depends_on, plan_id, plan_piece, awaiting_deploy, builder, build_session, trust`

type scanner interface{ Scan(...any) error }

func scanItem(row scanner) (pipeline.Item, error) {
	var (
		it                  pipeline.Item
		stage, ret          string
		resources, approval string
		trust               string
		dependsOn           string
		created, updated    int64
	)
	err := row.Scan(&it.ID, &it.Title, &it.Ask, &it.Repo, &it.Source, &it.Priority, &stage, &ret, &it.Question,
		&it.Area, &it.Size, &it.Risk, &resources, &it.BounceCount, &it.PR, &it.Branch, &it.HeadSHA,
		&it.BuilderAgent, &it.MergeSHA, &it.Summary, &approval, &created, &updated, &it.HoldReason, &it.NeedsQA, &it.ReopenOf, &it.CIRedSHA, &it.CIMainFixed,
		&dependsOn, &it.PlanID, &it.PlanPiece, &it.AwaitingDeploy, &it.Builder, &it.BuildSession, &trust)
	if err != nil {
		return it, err
	}
	it.Stage, it.ReturnStage = pipeline.Stage(stage), pipeline.Stage(ret)
	it.Resources = splitList(resources)
	if it.DependsOn, err = splitIDs(dependsOn); err != nil {
		return it, err
	}
	if approval != "" {
		it.Approval = &pipeline.Approval{}
		if err := json.Unmarshal([]byte(approval), it.Approval); err != nil {
			return it, err
		}
	}
	if trust != "" {
		it.Trust = &pipeline.Trust{}
		if err := json.Unmarshal([]byte(trust), it.Trust); err != nil {
			return it, err
		}
	}
	it.CreatedAt, it.UpdatedAt = fromNanos(created), fromNanos(updated)
	return it, nil
}

func splitList(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

func splitIDs(s string) ([]int64, error) {
	out := []int64{}
	for _, f := range splitList(s) {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("depends_on %q: %w", s, err)
		}
		out = append(out, id)
	}
	return out, nil
}

func joinIDs(ids []int64) string {
	fs := make([]string, len(ids))
	for i, id := range ids {
		fs[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(fs, ",")
}

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }

func loadItem(q interface {
	QueryRow(string, ...any) *sql.Row
}, id int64) (pipeline.Item, error) {
	it, err := scanItem(q.QueryRow(`SELECT `+itemCols+` FROM items WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return it, errf(CodeNotFound, "no item %d", id)
	}
	return it, err
}

func saveItem(tx *sql.Tx, it pipeline.Item) error {
	approval := ""
	if it.Approval != nil {
		b, err := json.Marshal(it.Approval)
		if err != nil {
			return err
		}
		approval = string(b)
	}
	trust := ""
	if it.Trust != nil {
		b, err := json.Marshal(it.Trust)
		if err != nil {
			return err
		}
		trust = string(b)
	}
	_, err := tx.Exec(`UPDATE items SET title=?, ask=?, repo=?, source=?, priority=?, stage=?, return_stage=?, question=?,
		area=?, size=?, risk=?, resources=?, bounce_count=?, pr=?, branch=?, head_sha=?, builder_agent=?, merge_sha=?,
		summary=?, approval=?, updated_at=?, hold_reason=?, needs_qa=?, ci_red_sha=?, ci_main_fixed=?, depends_on=?, awaiting_deploy=?, builder=?, build_session=?, trust=? WHERE id=?`,
		it.Title, it.Ask, it.Repo, it.Source, it.Priority, string(it.Stage), string(it.ReturnStage), it.Question,
		it.Area, it.Size, it.Risk, strings.Join(it.Resources, ","), it.BounceCount, it.PR, it.Branch, it.HeadSHA,
		it.BuilderAgent, it.MergeSHA, it.Summary, approval, it.UpdatedAt.UnixNano(), it.HoldReason, it.NeedsQA, it.CIRedSHA, it.CIMainFixed, joinIDs(it.DependsOn), it.AwaitingDeploy, it.Builder, it.BuildSession, trust, it.ID)
	return err
}

// NewItem is a request to add an item.
type NewItem struct {
	Title    string `json:"title"`
	Ask      string `json:"ask"`
	Repo     string `json:"repo"`
	Priority string `json:"priority"`
	Source   string `json:"source"`
	// DependsOn are existing items this one waits for (loop add --after, "/loop add … after #12").
	DependsOn []int64 `json:"depends_on"`
}

// AddItem adds an item at triage.
func (s *Store) AddItem(ctx context.Context, actor string, n NewItem) (pipeline.Item, error) {
	repo, err := s.ResolveRepo(n.Repo)
	if err != nil {
		return pipeline.Item{}, err
	}
	n.Repo = repo
	it, err := pipeline.NewItem(n.Title, n.Ask, n.Repo, n.Priority, n.Source, s.clock.Now())
	if err != nil {
		return it, err
	}
	it.DependsOn = dedupeIDs(n.DependsOn)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.insertItem(tx, actor, &it); err != nil {
			return err
		}
		// Checked once the item has its id, so naming the new item itself is refused.
		return checkDependencies(tx, it)
	})
	return it, err
}

// insertItem adds a new item and its created event, setting it.ID.
func (s *Store) insertItem(tx *sql.Tx, actor string, it *pipeline.Item) error {
	if it.DependsOn == nil {
		it.DependsOn = []int64{}
	}
	res, err := tx.Exec(`INSERT INTO items (title, ask, repo, source, priority, stage, risk, created_at, updated_at, reopen_of,
		depends_on, plan_id, plan_piece, builder) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.Title, it.Ask, it.Repo, it.Source, it.Priority, string(it.Stage), it.Risk, it.CreatedAt.UnixNano(), it.UpdatedAt.UnixNano(), it.ReopenOf,
		joinIDs(it.DependsOn), it.PlanID, it.PlanPiece, it.Builder)
	if err != nil {
		return err
	}
	if it.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	data := map[string]any{"title": it.Title, "priority": it.Priority, "source": it.Source, "repo": it.Repo}
	if it.ReopenOf != 0 {
		data["reopen_of"] = it.ReopenOf
	}
	if len(it.DependsOn) > 0 {
		data["depends_on"] = it.DependsOn
	}
	return s.event(tx, Event{ItemID: it.ID, Actor: actor, Kind: "created", To: string(it.Stage), Data: data})
}

// BoardRow is one line of the board.
type BoardRow struct {
	ID         int64     `json:"id"`
	Title      string    `json:"title"`
	Priority   string    `json:"priority"`
	Stage      string    `json:"stage"`
	Display    string    `json:"display"`
	Repo       string    `json:"repo,omitempty"`
	PR         int       `json:"pr,omitempty"`
	ClaimedBy  string    `json:"claimed_by,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	AgeSeconds int64     `json:"age_seconds"`
}

// ListFilter selects board rows. Empty fields match everything.
type ListFilter struct {
	Stage    string
	Priority string
	// Open leaves out done and closed items (the runner's snapshot for triage's dedupe).
	Open bool
	// PRToClose keeps the dropped items with a PR the runner has not closed yet (pr_closed).
	PRToClose bool
	// AwaitingDeploy keeps the qa items waiting for a deploy that contains their merge.
	AwaitingDeploy bool
}

// ListItems is the board, highest priority first, then oldest. Empty filters match everything.
func (s *Store) ListItems(ctx context.Context, stage, priority string) ([]BoardRow, error) {
	return s.ListItemsFilter(ctx, ListFilter{Stage: stage, Priority: priority})
}

// ListItemsFilter is ListItems with every filter.
func (s *Store) ListItemsFilter(ctx context.Context, f ListFilter) ([]BoardRow, error) {
	q := `SELECT i.id, i.title, i.priority, i.stage, i.repo, i.pr, i.created_at, COALESCE(l.runner || '/' || l.agent, '')
		FROM items i LEFT JOIN leases l ON l.item_id = i.id AND l.ended_at IS NULL WHERE 1=1`
	var args []any
	if f.Stage != "" {
		q += ` AND i.stage = ?`
		args = append(args, f.Stage)
	}
	if f.Priority != "" {
		q += ` AND i.priority = ?`
		args = append(args, f.Priority)
	}
	if f.Open {
		q += ` AND i.stage NOT IN ('done','closed')`
	}
	if f.PRToClose {
		q += ` AND i.stage = 'closed' AND i.pr > 0
			AND EXISTS (SELECT 1 FROM events e WHERE e.item_id = i.id AND e.kind = 'dropped')
			AND NOT EXISTS (SELECT 1 FROM events e WHERE e.item_id = i.id AND e.kind = 'pr_closed')`
	}
	if f.AwaitingDeploy {
		q += ` AND i.stage = 'qa' AND i.awaiting_deploy = 1`
	}
	q += ` ORDER BY i.priority, i.created_at, i.id`
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := s.clock.Now()
	out := []BoardRow{}
	for rows.Next() {
		var b BoardRow
		var created int64
		if err := rows.Scan(&b.ID, &b.Title, &b.Priority, &b.Stage, &b.Repo, &b.PR, &created, &b.ClaimedBy); err != nil {
			return nil, err
		}
		b.CreatedAt = fromNanos(created)
		b.AgeSeconds = int64(now.Sub(b.CreatedAt) / time.Second)
		b.Display = pipeline.Display(pipeline.Stage(b.Stage), b.ClaimedBy != "")
		out = append(out, b)
	}
	return out, rows.Err()
}

// Lease is a runner's hold on an item at a stage.
type Lease struct {
	ID        string         `json:"id"`
	ItemID    int64          `json:"item_id"`
	Stage     pipeline.Stage `json:"stage"`
	Runner    string         `json:"runner"`
	Agent     string         `json:"agent"`
	Resources []string       `json:"resources"`
	ClaimedAt time.Time      `json:"claimed_at"`
	ExpiresAt time.Time      `json:"expires_at"`
	Note      string         `json:"note,omitempty"`
	EndedAt   *time.Time     `json:"ended_at,omitempty"`
	EndReason string         `json:"end_reason,omitempty"`
	// Activity is what the run is doing, from its latest heartbeat (nil: none sent, or ended).
	Activity *pipeline.Activity `json:"activity,omitempty"`
}

const leaseCols = `id, item_id, stage, runner, agent, resources, claimed_at, expires_at, note, ended_at, end_reason, activity`

func scanLease(row scanner) (Lease, error) {
	var (
		l                Lease
		stage, resources string
		claimed, expires int64
		ended            sql.NullInt64
		activity         string
	)
	if err := row.Scan(&l.ID, &l.ItemID, &stage, &l.Runner, &l.Agent, &resources, &claimed, &expires, &l.Note, &ended, &l.EndReason, &activity); err != nil {
		return l, err
	}
	l.Activity = parseActivity(activity)
	l.Stage, l.Resources = pipeline.Stage(stage), splitList(resources)
	l.ClaimedAt, l.ExpiresAt = fromNanos(claimed), fromNanos(expires)
	if ended.Valid {
		t := fromNanos(ended.Int64)
		l.EndedAt = &t
	}
	return l, nil
}

// Detail is one item with its history.
type Detail struct {
	Item    pipeline.Item `json:"item"`
	Display string        `json:"display"`
	// WaitingOn is the items it depends on that are not done yet.
	WaitingOn []int64    `json:"waiting_on"`
	Lease     *Lease     `json:"lease,omitempty"`
	Events    []Event    `json:"events"`
	Evidence  []Evidence `json:"evidence"`
	// Frozen is the freeze on the item's repo while its main is red (the item may be its fix).
	Frozen *Freeze `json:"frozen,omitempty"`
}

// GetItem returns an item, its live lease, events and evidence.
func (s *Store) GetItem(ctx context.Context, id int64) (Detail, error) {
	var d Detail
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	if d.Item, err = loadItem(tx, id); err != nil {
		return d, err
	}
	l, err := scanLease(tx.QueryRow(`SELECT `+leaseCols+` FROM leases WHERE item_id = ? AND ended_at IS NULL`, id))
	switch {
	case err == nil:
		d.Lease = &l
	case !errors.Is(err, sql.ErrNoRows):
		return d, err
	}
	d.Display = pipeline.Display(d.Item.Stage, d.Lease != nil)
	deps, err := dependencyStages(tx, []pipeline.Item{d.Item})
	if err != nil {
		return d, err
	}
	d.WaitingOn = waitingOn(d.Item, deps)

	rows, err := tx.Query(`SELECT id, at, actor, kind, from_stage, to_stage, outcome, data FROM events WHERE item_id = ? ORDER BY id`, id)
	if err != nil {
		return d, err
	}
	d.Events = []Event{}
	for rows.Next() {
		e := Event{ItemID: id}
		var at int64
		var data string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Kind, &e.From, &e.To, &e.Outcome, &data); err != nil {
			rows.Close()
			return d, err
		}
		e.At = fromNanos(at)
		if err := json.Unmarshal([]byte(data), &e.Data); err != nil {
			rows.Close()
			return d, err
		}
		d.Events = append(d.Events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return d, err
	}
	if d.Evidence, err = listEvidence(tx, id); err != nil {
		return d, err
	}
	if d.Item.Repo != "" {
		d.Frozen, err = liveFreeze(tx, d.Item.Repo)
	}
	return d, err
}

// PatchItem changes priority or title.
func (s *Store) PatchItem(ctx context.Context, actor string, id int64, priority, title *string) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "patch", func(it pipeline.Item) (pipeline.Transition, error) {
		return pipeline.ApplyPatch(it, priority, title)
	})
}

// Answer records the user's answer to a needs_you item.
func (s *Store) Answer(ctx context.Context, actor string, id int64, text string) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "answer", func(it pipeline.Item) (pipeline.Transition, error) {
		return pipeline.ApplyAnswer(it, text)
	})
}

// Rebuild sends a held or needs_you item back to build with notes for the builder (the user's,
// or a runner's on a held PR that turned conflicting: a runner's moves only a held item, so its
// held watch never answers a question that came up meanwhile).
func (s *Store) Rebuild(ctx context.Context, actor string, id int64, notes string) (pipeline.Item, error) {
	return s.human(ctx, actor, id, "rebuild", func(it pipeline.Item) (pipeline.Transition, error) {
		if strings.HasPrefix(actor, "runner:") && it.Stage != pipeline.Held {
			return pipeline.Transition{}, &pipeline.Error{Code: pipeline.CodeWrongStage, Message: fmt.Sprintf("item is %s, not held", it.Stage)}
		}
		return pipeline.ApplyRebuild(it, notes)
	})
}

func (s *Store) human(ctx context.Context, actor string, id int64, action string, rule func(pipeline.Item) (pipeline.Transition, error)) (pipeline.Item, error) {
	return s.humanTx(ctx, actor, id, action, func(_ *sql.Tx, it pipeline.Item) (pipeline.Transition, error) { return rule(it) }, nil)
}

// humanTx is human with the transaction: rule may read other items, and then runs after the
// event is recorded (nil for nothing).
func (s *Store) humanTx(ctx context.Context, actor string, id int64, action string,
	rule func(*sql.Tx, pipeline.Item) (pipeline.Transition, error), then func(*sql.Tx) error) (pipeline.Item, error) {
	var out pipeline.Item
	err := s.tx(ctx, func(tx *sql.Tx) error {
		it, err := loadItem(tx, id)
		if err != nil {
			return err
		}
		tr, err := rule(tx, it)
		if err != nil {
			return err
		}
		tr.Item.UpdatedAt = s.clock.Now()
		if err := saveItem(tx, tr.Item); err != nil {
			return err
		}
		out = tr.Item
		if err := s.event(tx, transitionEvent(id, actor, tr)); err != nil {
			return err
		}
		if then != nil {
			if err := then(tx); err != nil {
				return err
			}
		}
		return s.planItemSettled(tx, tr.Item)
	})
	if err != nil {
		return out, s.refuse(ctx, actor, id, action, err)
	}
	return out, nil
}

// ---- claims and leases ----

// Claim is what a runner gets: the item, the stage it is to work, and the lease; or, with Kind
// plan, a plan turn (Plan, Turn) and its lease (stage "plan").
type Claim struct {
	Kind  string             `json:"kind"`
	Item  pipeline.Item      `json:"item"`
	Stage pipeline.Stage     `json:"stage"`
	Lease Lease              `json:"lease"`
	Plan  *pipeline.Plan     `json:"plan,omitempty"`
	Turn  *pipeline.PlanTurn `json:"turn,omitempty"`
	// Orch is an orchestrate turn's (Kind orchestrate, stage "orchestrate").
	Orch *OrchTurn `json:"orchestrate,omitempty"`
}

func newLeaseID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Claim gives runnerName the best waiting item (or, when req.Kinds has plan, a queued plan turn)
// for req, or nil when there is nothing to do.
func (s *Store) Claim(ctx context.Context, runnerName string, req pipeline.ClaimRequest) (*Claim, error) {
	var out *Claim
	err := s.tx(ctx, func(tx *sql.Tx) error {
		now := s.clock.Now()
		if err := s.sweep(tx); err != nil {
			return err
		}
		caps := append([]string(nil), req.Capabilities...)
		sort.Strings(caps)
		only := append([]string(nil), req.OnlyResources...)
		sort.Strings(only)
		var stages []string
		for _, st := range req.RunnerStages {
			stages = append(stages, string(st))
		}
		sort.Strings(stages)
		if _, err := tx.Exec(`INSERT INTO runners (name, capabilities, only_resources, poll_seconds, stages, last_seen) VALUES (?,?,?,?,?,?)
			ON CONFLICT (name) DO UPDATE SET capabilities = excluded.capabilities, only_resources = excluded.only_resources,
				poll_seconds = excluded.poll_seconds, stages = excluded.stages, last_seen = excluded.last_seen, state = ''`,
			runnerName, strings.Join(caps, ","), strings.Join(only, ","), max(req.PollSeconds, 0), strings.Join(stages, ","), now.UnixNano()); err != nil {
			return err
		}
		if err := s.checkUnservable(tx, now); err != nil {
			return err
		}
		cs, err := s.claimState(tx, now)
		if err != nil {
			return err
		}
		ctl, waiting, deps, snap := cs.ctl, cs.waiting, cs.deps, cs.live
		turns, err := turnCandidates(tx)
		if err != nil {
			return err
		}
		var orch *pipeline.OrchCandidate
		if slices.Contains(req.Kinds, pipeline.WorkOrchestrate) {
			if orch, err = s.orchCandidate(tx, ctl); err != nil {
				return err
			}
		}
		w, ok := pipeline.ChooseWork(ctl, waiting, deps, snap, turns, orch, cs.holds, req, cs.frozen)
		if !ok {
			return nil
		}
		if w.Orch != nil {
			out, err = s.claimOrch(tx, runnerName, req.Agent, *w.Orch)
			return err
		}
		if w.Turn != nil {
			out, err = s.claimTurn(tx, runnerName, req.Agent, *w.Turn)
			return err
		}
		it := *w.Item
		id, err := newLeaseID()
		if err != nil {
			return err
		}
		l := Lease{ID: id, ItemID: it.ID, Stage: it.Stage, Runner: runnerName, Agent: req.Agent,
			Resources: it.Resources, ClaimedAt: now, ExpiresAt: now.Add(LeaseTTL)}
		if _, err := tx.Exec(`INSERT INTO leases (id, item_id, stage, runner, agent, resources, claimed_at, expires_at) VALUES (?,?,?,?,?,?,?,?)`,
			l.ID, l.ItemID, string(l.Stage), l.Runner, l.Agent, strings.Join(l.Resources, ","), now.UnixNano(), l.ExpiresAt.UnixNano()); err != nil {
			return err
		}
		out = &Claim{Kind: pipeline.WorkItem, Item: it, Stage: it.Stage, Lease: l}
		return s.event(tx, Event{ItemID: it.ID, Actor: "runner:" + runnerName, Kind: "claimed", From: string(it.Stage), To: string(it.Stage),
			Data: map[string]any{"runner": runnerName, "agent": req.Agent, "lease": l.ID}})
	})
	return out, err
}

// claimState is what a claim chooses from: the control, the waiting items and their dependencies'
// stages, the live leases (items, plan and orchestrate turns), the frozen repos and the slots held
// (SlotHolds).
type claimState struct {
	ctl     pipeline.Control
	waiting []pipeline.Item
	deps    map[int64]pipeline.Stage
	live    []pipeline.LiveLease
	runners []pipeline.RunnerSeen
	frozen  pipeline.Frozen
	holds   []pipeline.SlotHold
}

func (s *Store) claimState(tx *sql.Tx, now time.Time) (claimState, error) {
	var cs claimState
	ctl, err := s.loadControl(tx)
	if err != nil {
		return cs, err
	}
	live, err := liveLeases(tx)
	if err != nil {
		return cs, err
	}
	snap := make([]pipeline.LiveLease, len(live))
	for i, l := range live {
		snap[i] = pipeline.LiveLease{ItemID: l.ItemID, Stage: l.Stage, Resources: l.Resources}
	}
	turnLeases, err := livePlanLeases(tx)
	if err != nil {
		return cs, err
	}
	for range turnLeases {
		snap = append(snap, pipeline.LiveLease{Stage: pipeline.PlanTurnStage})
	}
	running, err := orchTurnWith(tx, OrchRunning)
	if err != nil {
		return cs, err
	}
	if running != nil {
		snap = append(snap, pipeline.LiveLease{Stage: pipeline.OrchestrateStage})
	}
	waiting, err := waitingItems(tx)
	if err != nil {
		return cs, err
	}
	deps, err := dependencyStages(tx, waiting)
	if err != nil {
		return cs, err
	}
	runners, err := runnersSeen(tx)
	if err != nil {
		return cs, err
	}
	frozen, err := frozenRepos(tx)
	if err != nil {
		return cs, err
	}
	return claimState{ctl: ctl, waiting: waiting, deps: deps, live: snap, runners: runners, frozen: frozen,
		holds: pipeline.SlotHolds(ctl, waiting, deps, snap, runners, frozen, now)}, nil
}

// RunnerPanel is the board's runners: those seen within pipeline.RunnerSeenWindow, by name, and
// the slots held for their work now.
type RunnerPanel struct {
	Runners []pipeline.RunnerSeen
	Holds   []pipeline.SlotHold
}

// Runners is the board's runner panel.
func (s *Store) Runners(ctx context.Context) (RunnerPanel, error) {
	var p RunnerPanel
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	now := s.clock.Now()
	cs, err := s.claimState(tx, now)
	if err != nil {
		return p, err
	}
	for _, r := range cs.runners {
		if now.Sub(r.LastSeen) <= pipeline.RunnerSeenWindow {
			p.Runners = append(p.Runners, r)
		}
	}
	sort.Slice(p.Runners, func(i, j int) bool { return p.Runners[i].Name < p.Runners[j].Name })
	p.Holds = cs.holds
	return p, nil
}

func liveLeases(tx *sql.Tx) ([]Lease, error) {
	rows, err := tx.Query(`SELECT ` + leaseCols + ` FROM leases WHERE ended_at IS NULL ORDER BY claimed_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func waitingItems(tx *sql.Tx) ([]pipeline.Item, error) {
	rows, err := tx.Query(`SELECT ` + itemCols + ` FROM items WHERE stage IN ('triage','build','review','land')
		AND id NOT IN (SELECT item_id FROM leases WHERE ended_at IS NULL)
		AND id NOT IN (SELECT item_id FROM queued_advances WHERE applied_at IS NULL)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pipeline.Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func endLease(tx *sql.Tx, id string, at time.Time, reason string) error {
	_, err := tx.Exec(`UPDATE leases SET ended_at = ?, end_reason = ?, activity = '' WHERE id = ? AND ended_at IS NULL`, at.UnixNano(), reason, id)
	return err
}

// Sweep ends expired and overtime leases, and once a minute (of its clock) runs the orchestrator's
// timed triggers (OrchTick). loopd calls it on every request and once a minute.
func (s *Store) Sweep(ctx context.Context) error {
	now := s.clock.Now()
	s.tickMu.Lock()
	tick := s.tickAt.IsZero() || !now.Before(s.tickAt.Add(time.Minute))
	if tick {
		s.tickAt = now
	}
	s.tickMu.Unlock()
	if tick {
		// Unservable first, so the orchestrator's idle trigger skips what it already flagged.
		if err := s.tx(ctx, func(tx *sql.Tx) error { return s.checkUnservable(tx, now) }); err != nil {
			return err
		}
		if err := s.OrchTick(ctx); err != nil {
			return err
		}
	}
	// A cheap read first, so most requests do not take the write lock.
	ctl, err := s.loadControl(s.r)
	if err != nil {
		return err
	}
	var due int
	err = s.r.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM leases WHERE ended_at IS NULL AND (expires_at <= ?1 OR claimed_at <= ?2))
		+ (SELECT COUNT(*) FROM plan_leases WHERE ended_at IS NULL AND (expires_at <= ?1 OR claimed_at <= ?2))
		+ (SELECT COUNT(*) FROM orch_turns WHERE status = 'running' AND (expires_at <= ?1 OR claimed_at <= ?2))`,
		now.UnixNano(), now.Add(-ctl.MaxRun()).UnixNano()).Scan(&due)
	if err != nil || due == 0 {
		return err
	}
	return s.tx(ctx, s.sweep)
}

func (s *Store) sweep(tx *sql.Tx) error {
	now := s.clock.Now()
	ctl, err := s.loadControl(tx)
	if err != nil {
		return err
	}
	live, err := liveLeases(tx)
	if err != nil {
		return err
	}
	for _, l := range live {
		switch pipeline.JudgeLease(l.ClaimedAt, l.ExpiresAt, now, ctl.MaxRun()) {
		case pipeline.LeaseExpired:
			if err := endLease(tx, l.ID, now, "expired"); err != nil {
				return err
			}
			if err := s.event(tx, Event{ItemID: l.ItemID, Actor: "loopd", Kind: "lease_expired", From: string(l.Stage), To: string(l.Stage),
				Data: map[string]any{"lease": l.ID, "runner": l.Runner, "agent": l.Agent, "note": l.Note}}); err != nil {
				return err
			}
		case pipeline.LeaseOvertime:
			if err := endLease(tx, l.ID, now, "overtime"); err != nil {
				return err
			}
			it, err := loadItem(tx, l.ItemID)
			if err != nil {
				return err
			}
			tr := pipeline.Overtime(it, l.Note, ctl.MaxRun())
			tr.Item.UpdatedAt = now
			if err := saveItem(tx, tr.Item); err != nil {
				return err
			}
			tr.Data["lease"], tr.Data["runner"], tr.Data["agent"] = l.ID, l.Runner, l.Agent
			if err := s.event(tx, transitionEvent(l.ItemID, "loopd", tr)); err != nil {
				return err
			}
		}
	}
	if err := s.sweepPlans(tx, now, ctl.MaxRun()); err != nil {
		return err
	}
	return s.sweepOrch(tx, now, ctl.MaxRun())
}

// Heartbeat keeps a lease alive. stop is true when the runner should stop: control is stopped
// (the lease then ends and the item returns to its queue), or the lease has already ended.
func (s *Store) Heartbeat(ctx context.Context, runnerName, leaseID, note string) (stop bool, err error) {
	return s.HeartbeatActivity(ctx, runnerName, leaseID, note, nil)
}

// HeartbeatActivity is Heartbeat with what the run is doing, kept on the live lease (nil keeps
// the previous one, as an empty note does). The caller has validated it.
func (s *Store) HeartbeatActivity(ctx context.Context, runnerName, leaseID, note string, act *pipeline.Activity) (stop bool, err error) {
	actJSON, err := encodeActivity(act)
	if err != nil {
		return false, err
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx); err != nil {
			return err
		}
		now := s.clock.Now()
		l, err := scanLease(tx.QueryRow(`SELECT `+leaseCols+` FROM leases WHERE id = ?`, leaseID))
		if errors.Is(err, sql.ErrNoRows) {
			var found bool
			if stop, found, err = s.planHeartbeat(tx, runnerName, leaseID, note, actJSON); found || err != nil {
				return err
			}
			if stop, found, err = s.orchHeartbeat(tx, runnerName, leaseID, note, actJSON); found || err != nil {
				return err
			}
			return errf(CodeNotFound, "no lease %s", leaseID)
		}
		if err != nil {
			return err
		}
		if l.Runner != runnerName {
			return errf(CodeNotYourLease, "lease %s is held by another runner", leaseID)
		}
		if _, err := tx.Exec(`UPDATE runners SET last_seen = ? WHERE name = ?`, now.UnixNano(), runnerName); err != nil {
			return err
		}
		if l.EndedAt != nil {
			stop = true
			return nil
		}
		ctl, err := s.loadControl(tx)
		if err != nil {
			return err
		}
		if ctl.Stopped {
			stop = true
			if err := endLease(tx, l.ID, now, "stopped"); err != nil {
				return err
			}
			return s.event(tx, Event{ItemID: l.ItemID, Actor: "loopd", Kind: "stopped", From: string(l.Stage), To: string(l.Stage),
				Data: map[string]any{"lease": l.ID, "runner": l.Runner, "agent": l.Agent, "note": note}})
		}
		// A land run in a frozen repo stops before it can merge; the item waits in land.
		if l.Stage == pipeline.Land {
			it, err := loadItem(tx, l.ItemID)
			if err != nil {
				return err
			}
			frozen, err := frozenRepos(tx)
			if err != nil {
				return err
			}
			if frozen.Blocks(it) {
				stop = true
				if err := endLease(tx, l.ID, now, "frozen"); err != nil {
					return err
				}
				return s.event(tx, Event{ItemID: l.ItemID, Actor: "loopd", Kind: KindLandFrozen, From: string(l.Stage), To: string(l.Stage),
					Data: map[string]any{"lease": l.ID, "runner": l.Runner, "agent": l.Agent, "repo": it.Repo, "fix_item": frozen[it.Repo]}})
			}
		}
		if note == "" {
			note = l.Note
		}
		if actJSON == "" {
			actJSON = encodedActivity(l.Activity)
		}
		_, err = tx.Exec(`UPDATE leases SET expires_at = ?, note = ?, activity = ? WHERE id = ?`, now.Add(LeaseTTL).UnixNano(), note, actJSON, l.ID)
		return err
	})
	return stop, err
}

// heldLease returns the item's live lease if runnerName holds it.
func heldLease(tx *sql.Tx, id int64, runnerName string) (Lease, error) {
	l, err := scanLease(tx.QueryRow(`SELECT `+leaseCols+` FROM leases WHERE item_id = ? AND ended_at IS NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return l, errf(CodeNoLease, "item %d has no live lease", id)
	}
	if err != nil {
		return l, err
	}
	if l.Runner != runnerName {
		return l, errf(CodeNotYourLease, "item %d is leased by another runner", id)
	}
	return l, nil
}

// RunnerEvent records an event from inside a run (such as a guard refusal) on an item whose live
// lease runnerName holds.
func (s *Store) RunnerEvent(ctx context.Context, runnerName string, id int64, kind string, data map[string]any) (Event, error) {
	var out Event
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx); err != nil {
			return err
		}
		if _, err := loadItem(tx, id); err != nil {
			return err
		}
		l, err := heldLease(tx, id, runnerName)
		if err != nil {
			return err
		}
		if data == nil {
			data = map[string]any{}
		}
		data["lease"] = l.ID
		data["agent"] = l.Agent
		out = Event{ItemID: id, At: s.clock.Now(), Actor: "runner:" + runnerName, Kind: kind, From: string(l.Stage), To: string(l.Stage), Data: data}
		return s.event(tx, out)
	})
	return out, err
}

// Advance finishes the stage runnerName holds a lease on, with an outcome.
// changedPaths are the PR's changed files, required by pr_opened, merged and land's hold.
func (s *Store) Advance(ctx context.Context, runnerName string, id int64, outcome pipeline.Outcome, fields map[string]string, changedPaths []string) (pipeline.Item, error) {
	var out pipeline.Item
	actor := "runner:" + runnerName
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx); err != nil {
			return err
		}
		now := s.clock.Now()
		it, err := loadItem(tx, id)
		if err != nil {
			return err
		}
		l, err := heldLease(tx, id, runnerName)
		if err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM evidence WHERE lease_id = ?`, l.ID).Scan(&n); err != nil {
			return err
		}
		ctl, err := s.loadControl(tx)
		if err != nil {
			return err
		}
		adv := pipeline.Advance{Outcome: outcome, Fields: fields, Evidence: n, Agent: l.Agent,
			ChangedPaths: changedPaths, HoldRules: ctl.HoldRules, Repos: s.Repos()}
		tr, err := pipeline.ApplyAdvance(it, adv, now)
		if err != nil {
			return err
		}
		// In a repo whose main is red the run's result waits, checked now, applied once main is
		// green. A usage limit is about the runner's account, not the repo, and a merge has already
		// happened on GitHub (main's CI on it must be taken): both apply now.
		frozen, err := frozenRepos(tx)
		if err != nil {
			return err
		}
		if frozen.Blocks(it) && outcome != pipeline.UsageLimit && outcome != pipeline.Merged {
			out = it
			return s.queueAdvance(tx, actor, runnerName, it, l, adv, now)
		}
		if err := s.applyAdvance(tx, actor, id, l.ID, tr, fields, now); err != nil {
			return err
		}
		out = tr.Item
		return nil
	})
	if err != nil {
		return out, s.refuse(ctx, actor, id, "advance "+string(outcome), err)
	}
	return out, nil
}

// applyAdvance stores an advance's transition, made under leaseID (ended here when still live),
// and what follows from it.
func (s *Store) applyAdvance(tx *sql.Tx, actor string, id int64, leaseID string, tr pipeline.Transition, fields map[string]string, now time.Time) error {
	if err := saveItem(tx, tr.Item); err != nil {
		return err
	}
	if err := endLease(tx, leaseID, now, "advanced"); err != nil {
		return err
	}
	tr.Data["lease"] = leaseID
	if err := s.event(tx, transitionEvent(id, actor, tr)); err != nil {
		return err
	}
	for _, n := range tr.Also {
		// A note, not a transition: it stays in the stage the advance left the item in.
		if err := s.event(tx, Event{ItemID: id, Actor: actor, Kind: n.Kind, From: string(tr.To), To: string(tr.To), Data: n.Data}); err != nil {
			return err
		}
	}
	if tr.Item.Stage == pipeline.Closed {
		if err := s.dependencyClosed(tx, id); err != nil {
			return err
		}
	}
	if err := s.checkUnservable(tx, now); err != nil {
		return err
	}
	if err := s.planItemSettled(tx, tr.Item); err != nil {
		return err
	}
	if tr.ActiveAgentsZero {
		ctl, err := s.loadControl(tx)
		if err != nil {
			return err
		}
		before := ctl
		ctl.ActiveAgents = 0
		builtIn, err := builtInRules(tx)
		if err != nil {
			return err
		}
		if err := saveControl(tx, ctl, builtIn); err != nil {
			return err
		}
		if err := s.event(tx, Event{Actor: "loopd", Kind: "control", Data: map[string]any{
			"from": before, "to": ctl, "reason": "usage_limit", "item": id, "error": fields["error"], "reset": fields["reset"]}}); err != nil {
			return err
		}
	}
	return nil
}
