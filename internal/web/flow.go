package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The flow panel (docs/decisions/2026-10-18-flow-panel.md): GET /board/flow?window=6h|24h|7d,
// store.Flow as a page. The same numbers are GET /flow on the API.

func (p *boardPage) flow(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if window == "" {
		window = store.DefaultFlowWindow
	}
	f, err := p.st.Flow(r.Context(), window)
	if err != nil {
		if pe := (*pipeline.Error)(nil); errors.As(err, &pe) && pe.Code == pipeline.CodeInvalidField {
			http.Error(w, pe.Message, http.StatusBadRequest)
			return
		}
		p.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := flowTmpl.Execute(w, flowView(f)); err != nil {
		p.log.Error("render flow", "err", err)
	}
}

// Sparkline geometry: one bar per hour in a W×H box, bars Gap apart.
const (
	sparkW   = 600
	sparkH   = 48
	sparkGap = 2
)

type sparkBar struct {
	X, Y, W, H float64
	Title      string
}

type sparkline struct {
	Name  string
	Total int
	Max   int
	Bars  []sparkBar
	W, H  int
}

type flowData struct {
	store.Flow
	Windows    []string
	DoneRate   string // done per hour over the window
	MergedRate string
	Sparks     []sparkline
	BusyShare  string // busy agent-minutes as a share of the loop's capacity
}

func flowView(f store.Flow) flowData {
	v := flowData{Flow: f, Windows: store.FlowWindows}
	v.DoneRate, v.MergedRate = fmt.Sprintf("%.2f", float64(f.Done)/f.Hours), fmt.Sprintf("%.2f", float64(f.Merged)/f.Hours)
	v.Sparks = []sparkline{
		spark("Done", f.PerHour, func(h store.FlowHour) int { return h.Done }),
		spark("Merged", f.PerHour, func(h store.FlowHour) int { return h.Merged }),
	}
	if c := float64(f.ActiveAgents) * f.Hours * 60; c > 0 {
		v.BusyShare = fmt.Sprintf("%.0f%%", 100*f.BusyMinutes/c)
	}
	return v
}

func spark(name string, hours []store.FlowHour, n func(store.FlowHour) int) sparkline {
	s := sparkline{Name: name, W: sparkW, H: sparkH}
	for _, h := range hours {
		s.Total += n(h)
		s.Max = max(s.Max, n(h))
	}
	if len(hours) == 0 {
		return s
	}
	step := float64(sparkW) / float64(len(hours))
	bw := max(step-sparkGap, 1)
	for i, h := range hours {
		c := n(h)
		// An empty hour is a 1px stub on the baseline, so the hours stay countable.
		bh := 1.0
		if s.Max > 0 && c > 0 {
			bh = max(float64(c)/float64(s.Max)*float64(sparkH), 2)
		}
		s.Bars = append(s.Bars, sparkBar{X: float64(i) * step, Y: float64(sparkH) - bh, W: bw, H: bh,
			Title: fmt.Sprintf("%s: %d %s", h.Start.UTC().Format("Jan 2 15:04 UTC"), c, strings.ToLower(name))})
	}
	return s
}

// dur is a Dist's seconds as the panel shows them: "3h 05m", "12m", "40s".
func dur(secs int64) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd %02dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", secs)
}

// mins is agent-minutes, rounded to whole minutes.
func mins(m float64) string { return fmt.Sprintf("%.0f", m) }
