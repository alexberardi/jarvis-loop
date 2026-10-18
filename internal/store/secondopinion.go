package store

import (
	"context"
	"encoding/json"
)

// KindSecondOpinion is the runner's record of a Gemini second opinion on an approved review
// (runner/secondopinion.go): its status and counts, no finding text.
const KindSecondOpinion = "second_opinion"

// SecondOpinionStats are second opinions added up: for one item, or for all of them. Agreed
// counts only judged findings; the agreed rate is over judged ones.
type SecondOpinionStats struct {
	// Runs are the second opinions asked for; Unavailable those agy gave nothing usable for.
	Runs, Unavailable                   int
	Findings, Judged, Agreed            int
	High, Medium, Low                   int
	AgreedHigh, AgreedMedium, AgreedLow int
	// Bounced are the times blocking sent the item back to build.
	Bounced int
}

// AgreedPct is the agreed rate in percent (0 with nothing judged).
func (s SecondOpinionStats) AgreedPct() int {
	if s.Judged == 0 {
		return 0
	}
	return (s.Agreed*100 + s.Judged/2) / s.Judged
}

func (s *SecondOpinionStats) add(o SecondOpinionStats) {
	s.Runs += o.Runs
	s.Unavailable += o.Unavailable
	s.Findings += o.Findings
	s.Judged += o.Judged
	s.Agreed += o.Agreed
	s.High += o.High
	s.Medium += o.Medium
	s.Low += o.Low
	s.AgreedHigh += o.AgreedHigh
	s.AgreedMedium += o.AgreedMedium
	s.AgreedLow += o.AgreedLow
	s.Bounced += o.Bounced
}

// secondOpinionOf is one second_opinion event's numbers.
func secondOpinionOf(data map[string]any) SecondOpinionStats {
	n := func(k string) int {
		f, _ := data[k].(float64)
		return int(f)
	}
	s := SecondOpinionStats{Runs: 1, Findings: n("findings"), Judged: n("judged"), Agreed: n("agreed"),
		High: n("high"), Medium: n("medium"), Low: n("low"),
		AgreedHigh: n("agreed_high"), AgreedMedium: n("agreed_medium"), AgreedLow: n("agreed_low")}
	if data["status"] == "unavailable" {
		s.Unavailable = 1
	}
	if data["bounced"] == true {
		s.Bounced = 1
	}
	return s
}

// SumSecondOpinions adds up the second_opinion events among evs (an item's); false when there
// are none.
func SumSecondOpinions(evs []Event) (SecondOpinionStats, bool) {
	var s SecondOpinionStats
	for _, e := range evs {
		if e.Kind == KindSecondOpinion {
			s.add(secondOpinionOf(e.Data))
		}
	}
	return s, s.Runs > 0
}

// SecondOpinions adds up every second_opinion event: per item, and in total.
func (s *Store) SecondOpinions(ctx context.Context) (map[int64]SecondOpinionStats, SecondOpinionStats, error) {
	per := map[int64]SecondOpinionStats{}
	var total SecondOpinionStats
	rows, err := s.r.QueryContext(ctx, `SELECT item_id, data FROM events WHERE kind = ? AND item_id IS NOT NULL ORDER BY id`, KindSecondOpinion)
	if err != nil {
		return nil, total, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, total, err
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil, total, err
		}
		one := secondOpinionOf(data)
		st := per[id]
		st.add(one)
		per[id] = st
		total.add(one)
	}
	return per, total, rows.Err()
}
