package sandbox

import (
	"fmt"
	"sort"
	"strings"

	"github.com/alexberardi/jarvis-loop/internal/slack"
	"github.com/alexberardi/jarvis-loop/internal/slackfake"
)

// ChannelName names the two channels the bot may use; anything else shows its id.
func ChannelName(id string) string {
	switch id {
	case slack.ChannelLoop:
		return "#loop"
	case slack.ChannelDigest:
		return "#loop-digest"
	}
	return "#" + id + " (not an allowed channel)"
}

// DisplayName names a Slack user the way the transcript shows them.
func DisplayName(id string) string {
	switch id {
	case slackfake.BotUser:
		return "jarvis-loop"
	case slack.Approver:
		return "you"
	case Other:
		return "someone else"
	}
	return id
}

func plainText(v any) string {
	m, _ := v.(map[string]any)
	s, _ := m["text"].(string)
	return s
}

// elementText renders an interactive element: a button as [Label], a menu as [current ▾].
func elementText(el map[string]any) string {
	switch el["type"] {
	case "button":
		return "[" + plainText(el["text"]) + "]"
	case "static_select":
		cur := "choose"
		if io, ok := el["initial_option"].(map[string]any); ok {
			cur = plainText(io["text"])
		}
		return "[" + cur + " ▾]"
	}
	return ""
}

// Body is a message's visible content: its blocks (sections, then buttons and menus) or, without
// blocks, its text.
func Body(m slackfake.Message) string {
	if len(m.Blocks) == 0 {
		return m.Text
	}
	var lines []string
	for _, b := range m.Blocks {
		switch b["type"] {
		case "section":
			line := plainText(b["text"])
			if acc, ok := b["accessory"].(map[string]any); ok {
				if e := elementText(acc); e != "" {
					line += "  " + e
				}
			}
			lines = append(lines, line)
		case "actions":
			var els []string
			if list, ok := b["elements"].([]any); ok {
				for _, x := range list {
					if el, ok := x.(map[string]any); ok {
						if e := elementText(el); e != "" {
							els = append(els, e)
						}
					}
				}
			}
			lines = append(lines, strings.Join(els, " "))
		case "context":
			if list, ok := b["elements"].([]any); ok {
				for _, x := range list {
					lines = append(lines, plainText(x))
				}
			}
		}
	}
	return strings.Join(lines, "\n")
}

// Buttons lists a message's button labels and menu action ids.
func Buttons(m slackfake.Message) []string {
	var out []string
	add := func(el map[string]any) {
		switch el["type"] {
		case "button":
			out = append(out, plainText(el["text"]))
		case "static_select":
			out = append(out, fmt.Sprint(el["action_id"]))
		}
	}
	for _, b := range m.Blocks {
		if acc, ok := b["accessory"].(map[string]any); ok {
			add(acc)
		}
		if list, ok := b["elements"].([]any); ok {
			for _, x := range list {
				if el, ok := x.(map[string]any); ok {
					add(el)
				}
			}
		}
	}
	return out
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		if i > 0 {
			lines[i] = prefix + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func header(m slackfake.Message) string {
	who := DisplayName(m.User)
	var tags []string
	switch m.Kind {
	case "ephemeral", "response", "command":
		if m.To == slack.Approver {
			tags = append(tags, "only you see this")
		} else {
			tags = append(tags, "only "+DisplayName(m.To)+" sees this")
		}
	}
	if m.Edits > 0 {
		tags = append(tags, fmt.Sprintf("edited %d×", m.Edits))
	}
	if m.Pinned {
		tags = append(tags, "pinned")
	}
	h := who + ":"
	if len(tags) > 0 {
		h = who + " (" + strings.Join(tags, ", ") + "):"
	}
	return h
}

// Render prints recorded Slack messages as the threads would have looked: per channel, each
// top-level message (as it stands after its edits), then its replies indented. thread, when set,
// limits it to that one thread (its parent and replies).
func Render(msgs []slackfake.Message, thread string) string {
	byThread := map[string][]slackfake.Message{}
	var top []slackfake.Message
	for _, m := range msgs {
		if m.ThreadTS != "" && m.ThreadTS != m.TS {
			byThread[m.Channel+"/"+m.ThreadTS] = append(byThread[m.Channel+"/"+m.ThreadTS], m)
			continue
		}
		if thread != "" && m.TS != thread {
			continue
		}
		top = append(top, m)
	}
	channels := map[string][]slackfake.Message{}
	var order []string
	for _, m := range top {
		if _, ok := channels[m.Channel]; !ok {
			order = append(order, m.Channel)
		}
		channels[m.Channel] = append(channels[m.Channel], m)
	}
	sort.SliceStable(order, func(i, j int) bool { return rank(order[i]) < rank(order[j]) })
	var b strings.Builder
	for _, ch := range order {
		fmt.Fprintf(&b, "%s\n", ChannelName(ch))
		for _, m := range channels[ch] {
			fmt.Fprintf(&b, "  %s %s\n", header(m), indent(Body(m), "    "))
			for _, r := range byThread[ch+"/"+m.TS] {
				fmt.Fprintf(&b, "    ↳ %s %s\n", header(r), indent(Body(r), "      "))
			}
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func rank(ch string) int {
	switch ch {
	case slack.ChannelLoop:
		return 0
	case slack.ChannelDigest:
		return 1
	}
	return 2
}
