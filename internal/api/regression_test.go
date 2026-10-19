package api

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

func (e *env) uploadRegression(tok string, id int64, name string, content []byte, wantStatus int) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(content)
	mw.Close()
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/regressions/%d/files", e.srv.URL, id), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		e.t.Fatalf("upload: %d, want %d: %s", resp.StatusCode, wantStatus, raw)
	}
}

func TestRegressionRoutes(t *testing.T) {
	e := newEnv(t)
	// Only the user queues one, and one at a time.
	e.refused(runnerTok, "POST", "/regressions", map[string]any{"suite": "web"}, 403, "forbidden")
	var reg store.Regression
	e.do(userTok, "POST", "/regressions", map[string]any{"suite": "web"}, 201, &reg)
	if reg.Status != store.RegQueued || reg.Suite != "web" || reg.RequestedBy != "user:alex" {
		t.Fatalf("queued: %+v", reg)
	}
	e.refused(userTok, "POST", "/regressions", map[string]any{"suite": "all"}, 409, store.CodeRegressionOpen)

	// A runner gets it only when it asks for the kind.
	e.do(runnerTok, "POST", "/claim", map[string]any{"agent": "a", "kinds": []string{"item"}}, 204, nil)
	var cl struct {
		Kind  string            `json:"kind"`
		Stage string            `json:"stage"`
		Reg   *store.Regression `json:"regression"`
		Lease struct {
			ID string `json:"id"`
		} `json:"lease"`
	}
	e.do(runnerTok, "POST", "/claim", map[string]any{"agent": "a", "kinds": []string{"item", "regression"}}, 200, &cl)
	if cl.Kind != pipeline.WorkRegression || cl.Stage != "regression" || cl.Reg == nil || cl.Reg.ID != reg.ID || cl.Lease.ID == "" {
		t.Fatalf("claim: %+v", cl)
	}
	e.do(runnerTok, "POST", "/leases/"+cl.Lease.ID+"/heartbeat", map[string]any{"note": "e2e web"}, 200, nil)

	png := []byte("\x89PNG\r\n\x1a\nshot")
	e.uploadRegression(otherTok, reg.ID, "shot.png", png, 403)
	e.uploadRegression(runnerTok, reg.ID, "shot.png", png, 201)
	path := fmt.Sprintf("/regressions/%d/result", reg.ID)
	res := map[string]any{"server_head": shaA, "tests": []map[string]any{
		{"suite": "web", "title": "ok", "status": "passed"},
		{"suite": "web", "title": "login › bad password", "status": "failed", "error": "expected Wrong password", "screenshot": "shot.png"},
	}}
	e.refused(userTok, "POST", path, res, 403, "forbidden")
	e.do(runnerTok, "POST", path, res, 200, &reg)
	if reg.Status != store.RegFailed || reg.Passed != 1 || reg.Failed != 1 {
		t.Fatalf("result: %+v", reg)
	}

	var full store.Regression
	e.do(runnerTok, "GET", fmt.Sprintf("/regressions/%d", reg.ID), nil, 200, &full)
	if len(full.Tests) != 2 || full.Tests[1].Screenshot != 1 || len(full.Files) != 1 {
		t.Fatalf("get: %+v", full)
	}
	var list struct {
		Regressions []store.Regression `json:"regressions"`
	}
	e.do(userTok, "GET", "/regressions", nil, 200, &list)
	if len(list.Regressions) != 1 {
		t.Fatalf("list: %+v", list)
	}

	// File it: the user's, once per test.
	file := fmt.Sprintf("/regressions/%d/tests/2/file", reg.ID)
	e.refused(runnerTok, "POST", file, nil, 403, "forbidden")
	var filed struct {
		Item     pipeline.Item `json:"item"`
		Existing bool          `json:"existing"`
	}
	e.do(userTok, "POST", file, nil, 201, &filed)
	if filed.Existing || filed.Item.ID == 0 {
		t.Fatalf("filed: %+v", filed)
	}
	first := filed.Item.ID
	e.do(userTok, "POST", file, nil, 200, &filed)
	if !filed.Existing || filed.Item.ID != first {
		t.Fatalf("filed again: %+v", filed)
	}
	if d := e.get(first); len(d.Evidence) != 1 {
		t.Fatalf("evidence: %+v", d.Evidence)
	}
}

func TestScheduledRegressionRoutes(t *testing.T) {
	e := newEnv(t)
	e.refused(runnerTok, "POST", "/regressions/scheduled", nil, 403, "forbidden")
	var reg store.Regression
	e.do(userTok, "POST", "/regressions/scheduled", nil, 201, &reg)
	if reg.Status != store.RegScheduled || reg.Suite != pipeline.SuiteOvernight || reg.NotBefore == nil {
		t.Fatalf("scheduled: %+v", reg)
	}
	e.refused(userTok, "POST", "/regressions/scheduled", nil, 409, store.CodeRegressionOpen)
	var got struct {
		Regression *store.Regression `json:"regression"`
	}
	e.do(userTok, "GET", "/regressions/scheduled", nil, 200, &got)
	if got.Regression == nil || got.Regression.ID != reg.ID {
		t.Fatalf("GET scheduled: %+v", got)
	}
	e.refused(runnerTok, "DELETE", "/regressions/scheduled", nil, 403, "forbidden")
	e.do(userTok, "DELETE", "/regressions/scheduled", nil, 200, &reg)
	if reg.Status != store.RegCancelled {
		t.Fatalf("cancelled: %+v", reg)
	}
	e.refused(userTok, "DELETE", "/regressions/scheduled", nil, 404, store.CodeNotFound)
	// The nightly setting is the control's.
	var ctl pipeline.Control
	e.do(userTok, "PUT", "/control", map[string]any{"regression_nightly": true, "regression_at": "02:00"}, 200, &ctl)
	if !ctl.RegressionNightly || ctl.RegressionAt != "02:00" {
		t.Fatalf("control %+v", ctl)
	}
	e.refused(userTok, "PUT", "/control", map[string]any{"regression_at": "2am"}, 400, pipeline.CodeInvalidField)
}
