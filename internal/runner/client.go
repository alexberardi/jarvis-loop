package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
)

// Client talks to loopd with the runner's token. The agent never sees it.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// APIError is a loopd error answer.
type APIError struct {
	Status int
	Code   string `json:"code"`
	Msg    string `json:"error"`
}

func (e *APIError) Error() string { return fmt.Sprintf("loopd %d %s: %s", e.Status, e.Code, e.Msg) }

// Claim is loopd's answer to a claim: an item in a stage, or (Kind plan) a plan's turn.
type Claim struct {
	Kind  string `json:"kind"`
	Item  Item   `json:"item"`
	Stage string `json:"stage"`
	// Plan and Turn are a plan turn's (Kind plan).
	Plan *pipeline.Plan     `json:"plan,omitempty"`
	Turn *pipeline.PlanTurn `json:"turn,omitempty"`
	// Orch is an orchestrate turn's (Kind orchestrate).
	Orch *OrchTurn `json:"orchestrate,omitempty"`
	// Reg is a regression run's (Kind regression).
	Reg   *Regression `json:"regression,omitempty"`
	Lease struct {
		ID        string    `json:"id"`
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"lease"`
}

// Control is the part of loopd's control the runner reads.
type Control struct {
	ActiveAgents  int  `json:"active_agents"`
	Stopped       bool `json:"stopped"`
	MaxRunMinutes int  `json:"max_run_minutes"`
	// HoldRules are loopd's hold rules: land applies them to the paths it computed before it
	// merges, as loopd does after.
	HoldRules []pipeline.HoldRule `json:"hold_rules"`
}

// NewItem is an item the runner files (land's P1 for a red main).
type NewItem struct {
	Title    string `json:"title"`
	Ask      string `json:"ask"`
	Repo     string `json:"repo"`
	Priority string `json:"priority"`
	Source   string `json:"source"`
}

// AddItem files an item (POST /items) and returns its id.
func (c *Client) AddItem(ctx context.Context, n NewItem) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	_, err := c.json(ctx, "POST", "/items", n, &out)
	return out.ID, err
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body io.Reader, out any) (int, error) {
	resp, err := c.send(ctx, method, path, contentType, body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		ae := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(raw, ae) != nil || ae.Code == "" {
			ae.Code, ae.Msg = "http", strings.TrimSpace(string(raw))
		}
		return resp.StatusCode, ae
	}
	if out != nil && resp.StatusCode != http.StatusNoContent && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("loopd %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func (c *Client) json(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	return c.do(ctx, method, path, ct, body, out)
}

// Claim asks for work in the given stages; nil when there is none. Stages plan and orchestrate
// are not item stages: they ask for plan or orchestrate turns too (kinds), and alone, for those
// only.
func (c *Client) Claim(ctx context.Context, agent string, caps, stages []string) (*Claim, error) {
	return c.claim(ctx, agent, caps, nil, stages, 0, nil, nil, 0, nil)
}

// ClaimFor is the runner's claim in stages under cfg, the config of this claim (a reload
// replaces it): its capabilities and only_resources, its slots (how many runs it works at once,
// which loopd records for the flow panel's idle capacity), and, for loopd's slot holds, its poll
// interval (it holds a slot for a Mac runner's work only while the runner has asked within two
// of them) and its configured stages (the work ones, from either lane: it holds a slot only for
// work the runner would take in them). It sends the names of its repos (Config.RepoNames), so
// loopd gives it only items it has a checkout of (#467).
//
// A runner with qwen (cfg.Qwen) claiming builds says it can build with it too
// (pipeline.BuilderQwen): a paced claim may route a build there.
func (c *Client) ClaimFor(ctx context.Context, agent string, cfg *Config, stages []string) (*Claim, error) {
	var builders []string
	if cfg.Qwen != nil && (len(stages) == 0 || slices.Contains(stages, StageBuild)) {
		builders = []string{pipeline.BuilderQwen}
	}
	return c.claim(ctx, agent, cfg.Capabilities, cfg.OnlyResources, stages, cfg.PollSeconds, cfg.Stages, builders, cfg.Slots, cfg.RepoNames())
}

func (c *Client) claim(ctx context.Context, agent string, caps, only, stages []string, pollSeconds int, cfgStages, builders []string, slots int, repos []string) (*Claim, error) {
	body := map[string]any{"agent": agent, "capabilities": caps, "stages": stages}
	if len(repos) > 0 {
		body["repos"] = repos
	}
	if len(only) > 0 {
		body["only_resources"] = only
	}
	if len(builders) > 0 {
		body["builders"] = builders
	}
	if slots > 0 {
		body["slots"] = slots
	}
	if pollSeconds > 0 {
		body["poll_seconds"] = pollSeconds
	}
	var runnerStages []string
	for _, st := range cfgStages {
		if pipeline.Stage(st).IsWork() {
			runnerStages = append(runnerStages, st)
		}
	}
	if len(runnerStages) > 0 {
		body["runner_stages"] = runnerStages
	}
	// The kinds from the config, so a land-lane claim does not tell loopd the runner takes items only.
	if _, kinds := claimKinds(cfgStages); len(kinds) > 0 {
		body["runner_kinds"] = kinds
	}
	if items, kinds := claimKinds(stages); len(kinds) > 0 && !slices.Equal(kinds, []string{pipeline.WorkItem}) {
		body["stages"], body["kinds"] = items, kinds
	}
	var cl Claim
	st, err := c.json(ctx, "POST", "/claim", body, &cl)
	if err != nil {
		return nil, err
	}
	if st == http.StatusNoContent {
		return nil, nil
	}
	return &cl, nil
}

var turnKinds = map[string]string{StagePlan: pipeline.WorkPlan, StageOrchestrate: pipeline.WorkOrchestrate, StageRegression: pipeline.WorkRegression}

// claimKinds splits stages into the item stages and the kinds of work they take (item first,
// when there are item stages).
func claimKinds(stages []string) (items, kinds []string) {
	for _, s := range stages {
		if k := turnKinds[s]; k != "" {
			kinds = append(kinds, k)
		} else {
			items = append(items, s)
		}
	}
	if len(items) > 0 {
		kinds = append([]string{pipeline.WorkItem}, kinds...)
	}
	return items, kinds
}

// OrchTurnResult finishes the orchestrate turn this runner holds the lease on.
func (c *Client) OrchTurnResult(ctx context.Context, turn int64, r OrchTurnResult) error {
	_, err := c.json(ctx, "POST", fmt.Sprintf("/orchestrate/turns/%d/result", turn), r, nil)
	return err
}

// Snapshot is loopd's snapshot for the orchestrator (with the orchestrator's token), as loopd
// sent it.
func (c *Client) Snapshot(ctx context.Context) ([]byte, error) {
	var raw json.RawMessage
	if _, err := c.json(ctx, "GET", "/orchestrator/snapshot", nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// PlanTurnResult finishes the plan turn this runner holds the lease on.
func (c *Client) PlanTurnResult(ctx context.Context, plan, turn int64, r pipeline.TurnResult) (pipeline.PlanTurn, error) {
	var out struct {
		Turn pipeline.PlanTurn `json:"turn"`
	}
	_, err := c.json(ctx, "POST", fmt.Sprintf("/plans/%d/turns/%d/result", plan, turn), r, &out)
	return out.Turn, err
}

// PlanDoc is a plan's latest document of a kind (brief, blueprint); nil when it has none yet.
func (c *Client) PlanDoc(ctx context.Context, plan int64, kind string) (*pipeline.PlanDoc, error) {
	return c.PlanDocVersion(ctx, plan, kind, 0)
}

// PlanDocVersion is version v of a plan's document of a kind (0: the latest); nil when there is
// no such version.
func (c *Client) PlanDocVersion(ctx context.Context, plan int64, kind string, v int) (*pipeline.PlanDoc, error) {
	var d pipeline.PlanDoc
	path := fmt.Sprintf("/plans/%d/docs/%s", plan, kind)
	if v > 0 {
		path += fmt.Sprintf("?v=%d", v)
	}
	_, err := c.json(ctx, "GET", path, nil, &d)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// Plan is a plan (its stage and approved versions).
func (c *Client) Plan(ctx context.Context, id int64) (pipeline.Plan, error) {
	var out struct {
		Plan pipeline.Plan `json:"plan"`
	}
	_, err := c.json(ctx, "GET", fmt.Sprintf("/plans/%d", id), nil, &out)
	return out.Plan, err
}

// Heartbeat keeps a lease alive, with what the run is doing (nil: unchanged); true means stop.
func (c *Client) Heartbeat(ctx context.Context, lease, note string, act *pipeline.Activity) (bool, error) {
	var out struct {
		Stop bool `json:"stop"`
	}
	body := map[string]any{"note": note}
	if act != nil {
		body["activity"] = act
	}
	_, err := c.json(ctx, "POST", "/leases/"+lease+"/heartbeat", body, &out)
	var ae *APIError
	if act != nil && errors.As(err, &ae) && ae.Status == http.StatusBadRequest {
		// A loopd from before activity (or one that refuses this one): the lease still needs
		// its heartbeat.
		_, err = c.json(ctx, "POST", "/leases/"+lease+"/heartbeat", map[string]any{"note": note}, &out)
	}
	if err != nil {
		return false, err
	}
	return out.Stop, nil
}

// SetState tells loopd the runner's state: RunnerDraining, or "" (working; a claim says so too).
func (c *Client) SetState(ctx context.Context, state string) error {
	_, err := c.json(ctx, "POST", "/runners/self/state", map[string]any{"state": state}, nil)
	return err
}

// Advance finishes the stage with an outcome.
func (c *Client) Advance(ctx context.Context, id int64, outcome string, fields map[string]string, paths []string) error {
	_, err := c.json(ctx, "POST", fmt.Sprintf("/items/%d/advance", id),
		map[string]any{"outcome": outcome, "fields": fields, "changed_paths": paths}, nil)
	return err
}

// Event records an event from inside the run (a guard refusal).
func (c *Client) Event(ctx context.Context, id int64, kind string, data map[string]any) error {
	_, err := c.json(ctx, "POST", fmt.Sprintf("/items/%d/events", id), map[string]any{"kind": kind, "data": data}, nil)
	return err
}

// Control reads loopd's control.
func (c *Client) Control(ctx context.Context) (Control, error) {
	var ctl Control
	_, err := c.json(ctx, "GET", "/control", nil, &ctl)
	return ctl, err
}

// Upload streams one evidence file (label first, then the file, as loopd wants it).
func (c *Client) Upload(ctx context.Context, id int64, path, name, label string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if label != "" {
				if err := mw.WriteField("label", label); err != nil {
					return err
				}
			}
			fw, err := mw.CreateFormFile("file", name)
			if err != nil {
				return err
			}
			if _, err := io.Copy(fw, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	_, err = c.do(ctx, "POST", fmt.Sprintf("/items/%d/evidence", id), mw.FormDataContentType(), pr, nil)
	return err
}

// EvidenceInfo is one evidence file on an item.
type EvidenceInfo struct {
	N     int    `json:"n"`
	Name  string `json:"name"`
	Label string `json:"label"`
	Size  int64  `json:"size"`
	// LeaseID is the lease (the run) it was attached under.
	LeaseID string `json:"lease_id"`
}

// EventInfo is one event in an item's history.
type EventInfo struct {
	At      time.Time      `json:"at"`
	Actor   string         `json:"actor"`
	Kind    string         `json:"kind"`
	From    string         `json:"from"`
	To      string         `json:"to"`
	Outcome string         `json:"outcome"`
	Data    map[string]any `json:"data"`
}

// Detail is an item with its history and evidence.
type Detail struct {
	Item     Item           `json:"item"`
	Events   []EventInfo    `json:"events"`
	Evidence []EvidenceInfo `json:"evidence"`
	// FollowedBy are the open items filed after it (they depend on it).
	FollowedBy []int64 `json:"followed_by,omitempty"`
	// Frozen is set while main is red in the item's repo.
	Frozen *Freeze `json:"frozen,omitempty"`
}

// Freeze is loopd's freeze of a repo whose main is red: ItemID is its fix, the one item that moves.
type Freeze struct {
	Repo   string `json:"repo"`
	ItemID int64  `json:"item_id"`
}

// Detail reads one item with its events and evidence.
func (c *Client) Detail(ctx context.Context, id int64) (Detail, error) {
	var d Detail
	_, err := c.json(ctx, "GET", fmt.Sprintf("/items/%d", id), nil, &d)
	return d, err
}

// Evidence lists an item's evidence files, oldest first.
func (c *Client) Evidence(ctx context.Context, id int64) ([]EvidenceInfo, error) {
	d, err := c.Detail(ctx, id)
	return d.Evidence, err
}

// BoardRow is one open item, for triage's snapshot.
type BoardRow struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Priority string `json:"priority"`
	Stage    string `json:"stage"`
	Display  string `json:"display"`
	Repo     string `json:"repo"`
	PR       int    `json:"pr"`
	// WaitPRs are a waiting item's PRs not seen merged yet (owner/name#444).
	WaitPRs []string `json:"wait_prs,omitempty"`
}

// OpenItems lists the open items (GET /items?open=1).
func (c *Client) OpenItems(ctx context.Context) ([]BoardRow, error) {
	var out struct {
		Items []BoardRow `json:"items"`
	}
	_, err := c.json(ctx, "GET", "/items?open=1", nil, &out)
	return out.Items, err
}

// PRsToClose lists the dropped items whose PR no runner has closed yet (GET /items?pr_to_close=1).
func (c *Client) PRsToClose(ctx context.Context) ([]BoardRow, error) {
	var out struct {
		Items []BoardRow `json:"items"`
	}
	_, err := c.json(ctx, "GET", "/items?pr_to_close=1", nil, &out)
	return out.Items, err
}

// AwaitingDeploy lists the qa items waiting for a deploy that contains their merge
// (GET /items?awaiting_deploy=1).
func (c *Client) AwaitingDeploy(ctx context.Context) ([]BoardRow, error) {
	var out struct {
		Items []BoardRow `json:"items"`
	}
	_, err := c.json(ctx, "GET", "/items?awaiting_deploy=1", nil, &out)
	return out.Items, err
}

// ProofsDue lists the qa items whose proof is due a check (GET /items?proof_due=1).
func (c *Client) ProofsDue(ctx context.Context) ([]BoardRow, error) {
	var out struct {
		Items []BoardRow `json:"items"`
	}
	_, err := c.json(ctx, "GET", "/items?proof_due=1", nil, &out)
	return out.Items, err
}

// Held lists the held items (GET /items?stage=held).
func (c *Client) Held(ctx context.Context) ([]BoardRow, error) {
	var out struct {
		Items []BoardRow `json:"items"`
	}
	_, err := c.json(ctx, "GET", "/items?stage=held", nil, &out)
	return out.Items, err
}

// Waiting lists the waiting items (GET /items?stage=waiting).
func (c *Client) Waiting(ctx context.Context) ([]BoardRow, error) {
	var out struct {
		Items []BoardRow `json:"items"`
	}
	_, err := c.json(ctx, "GET", "/items?stage=waiting", nil, &out)
	return out.Items, err
}

// WaitPR reports a PR a waiting item waits on merged or closed (POST /items/{id}/wait-pr).
func (c *Client) WaitPR(ctx context.Context, id int64, pr, state string) error {
	_, err := c.json(ctx, "POST", fmt.Sprintf("/items/%d/wait-pr", id), map[string]string{"pr": pr, "state": state}, nil)
	return err
}

// Rebuild sends a held item back to build with notes for the builder (POST /items/{id}/rebuild).
func (c *Client) Rebuild(ctx context.Context, id int64, notes string) error {
	_, err := c.json(ctx, "POST", fmt.Sprintf("/items/%d/rebuild", id), map[string]string{"notes": notes}, nil)
	return err
}

// HeldMerge reports the runner's merge of the base into a held item's PR: the new head goes to
// review (POST /items/{id}/held-merge).
func (c *Client) HeldMerge(ctx context.Context, id int64, head, notes string) error {
	_, err := c.json(ctx, "POST", fmt.Sprintf("/items/%d/held-merge", id), map[string]string{"head_sha": head, "notes": notes}, nil)
	return err
}

// DownloadEvidence saves evidence file n of an item to dst (at most MaxEvidenceBytes).
func (c *Client) DownloadEvidence(ctx context.Context, id int64, n int, dst string) error {
	resp, err := c.send(ctx, "GET", fmt.Sprintf("/items/%d/evidence/%d", id, n), "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Status: resp.StatusCode, Code: "http", Msg: strings.TrimSpace(string(b))}
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	n64, err := io.Copy(f, io.LimitReader(resp.Body, MaxEvidenceBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n64 > MaxEvidenceBytes {
		err = fmt.Errorf("larger than 20 MB")
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}
