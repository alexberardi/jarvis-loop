package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The runway warning (#205): loopd's runway_low event, posted once per dry spell in #loop by the
// loop bot, so the Approve and Trust buttons it carries are the same ones (and the same approver
// check) as on the item's own messages.

// runwayBlock prefixes the block_id of a runway warning's buttons: pressing one must not replace
// the whole warning (the other unblockers stay).
const runwayBlock = "runway:"

type runwayUnblocker struct {
	Item   int64  `json:"item"`
	Action string `json:"action"`
	Frees  int    `json:"frees"`
	SHA    string `json:"sha"`
}

// runwayMessage is the warning: how long the loop can run without the user, then what frees the
// most, each with its button.
func (b *Bot) runwayMessage(ctx context.Context, e store.Event) Message {
	var ubs []runwayUnblocker
	if raw, err := json.Marshal(e.Data["unblockers"]); err == nil {
		json.Unmarshal(raw, &ubs)
	}
	hours, _ := e.Data["hours"].(float64)
	lead := fmt.Sprintf("<@%s> :hourglass: *The loop runs dry in about %.1f agent-hours*: that is all it can do without you.", Approver, hours)
	if hours == 0 {
		lead = fmt.Sprintf("<@%s> :hourglass: *The loop has run dry*: nothing it can do without you.", Approver)
	}
	head := fmt.Sprintf("%s These %d unblock the most work, before you go:", lead, len(ubs))
	if len(ubs) == 1 {
		head = lead + " This unblocks the most work, before you go:"
	}
	blocks := []map[string]any{section(head)}
	var lines []string
	for _, u := range ubs {
		line := runwayLine(b.refID(ctx, u.Item), u)
		lines = append(lines, fmt.Sprintf("%s #%d → frees %d", u.Action, u.Item, u.Frees))
		value := fmt.Sprintf("%d:%s", u.Item, u.SHA)
		var btn map[string]any
		switch u.Action {
		case pipeline.UnblockApprove:
			btn = button("Approve merge", "approve", value, "primary")
		case pipeline.UnblockTrust:
			btn = button("Trust this commit", "trust", value, "primary")
		default:
			line += " (reply in its thread)"
		}
		blocks = append(blocks, section(line))
		if btn != nil && u.SHA != "" {
			blocks = append(blocks, map[string]any{"type": "actions", "block_id": runwayBlock + strconv.FormatInt(u.Item, 10), "elements": []any{btn}})
		}
	}
	return Message{Channel: ChannelLoop, Text: lead + " " + strings.Join(lines, "; "), Blocks: blocks}
}

// runwayLine is one unblocker: "approve #184 title → frees 4".
func runwayLine(ref string, u runwayUnblocker) string {
	what := map[string]string{
		pipeline.UnblockApprove: "Approve",
		pipeline.UnblockTrust:   "Trusted-file OK on",
		pipeline.UnblockAnswer:  "Answer",
	}[u.Action]
	if what == "" {
		what = Escape(u.Action)
	}
	at := ""
	if u.SHA != "" {
		at = fmt.Sprintf(" at `%s`", short(u.SHA))
	}
	return fmt.Sprintf("• %s %s%s → *frees %d*", what, ref, at, u.Frees)
}
