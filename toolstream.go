package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// A run event is one JSON object per SSE data frame. Only tool.started is
// rendered; the other kinds (output deltas, reasoning, subagent lifecycle) are
// ignored on purpose.
type runEvent struct {
	Event   string `json:"event"`
	Tool    string `json:"tool"`
	Preview string `json:"preview"`
	Seq     int    `json:"seq"`
}

// toolSummaryLimit caps how many summaries one turn may add, so a long-running
// agent cannot flood a room. The reply outbox drains one message per second, so
// the cap also bounds how far a burst of tool calls can delay the answer. The
// overflow is stated once instead of silently dropped.
const toolSummaryLimit = 10

// toolSummaryWait bounds how long the answer waits for the tool stream to
// drain, so a wedged Hermes gateway cannot hold the reply back.
const toolSummaryWait = 2 * time.Second

// toolStreamClient reads the run event stream. It carries no total timeout — the
// stream is meant to stay open for the whole turn and the server closes it when
// the run ends — and it refuses redirects, so the agent's API key is never
// replayed to another host.
var toolStreamClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// attachToolStream subscribes to the run's event stream and mirrors every
// tool.started frame into the room as a one-line summary. The returned channel
// closes once the stream has ended and every summary has been queued.
func (e *Engine) attachToolStream(ctx context.Context, id, turn, endpoint, runID string, from int) <-chan struct{} {
	done := make(chan struct{})
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer close(done)
		e.streamTools(ctx, id, turn, endpoint, runID, from)
	}()
	return done
}

// streamTools reads the run's event stream until it ends. It is best effort: a
// stream that cannot be opened, or that fails midway, only costs the summaries
// it had not delivered yet. The turn's answer is never affected.
func (e *Engine) streamTools(ctx context.Context, id, turn, endpoint, runID string, from int) {
	if runID == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+"/v1/runs/"+url.PathEscape(runID)+"/events", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+e.mx.key(id))
	req.Header.Set("Accept", "text/event-stream")
	if from > 0 {
		// The server replays the events after this sequence number.
		req.Header.Set("Last-Event-ID", strconv.Itoa(from-1))
	}
	resp, err := toolStreamClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			if ev, ok := parseRunEvent(line); ok && ev.Event == "tool.started" {
				e.queueToolSummary(id, turn, ev)
			}
		}
		if err != nil {
			return
		}
	}
}

// parseRunEvent decodes one SSE data frame. The server sends a single JSON
// object per frame; its id: and comment lines are ignored.
func parseRunEvent(line string) (runEvent, bool) {
	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		return runEvent{}, false
	}
	var ev runEvent
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return runEvent{}, false
	}
	return ev, true
}

// toolSummary renders one tool call as a single line: the tool name and the
// argument preview the agent produced. The call's output is never included.
func toolSummary(ev runEvent) string {
	name := shortText(ev.Tool, 40)
	if name == "" {
		return ""
	}
	preview := shortText(ev.Preview, 160)
	if preview == "" {
		return "🔧 " + name
	}
	return "🔧 " + name + ": " + preview
}

// queueToolSummary queues one summary for the turn. The turn's event cursor and
// its summary count advance in the same state write, so a controller restart
// resumes the stream without repeating a line, and the per-turn cap holds
// across a reconnect. A summary is plain text: a tool argument is
// agent-supplied and must not be rendered as markdown.
func (e *Engine) queueToolSummary(id, turn string, ev runEvent) {
	text := e.redact(toolSummary(ev))
	_ = e.s.Update(func(st *State) error {
		m := st.Members[id]
		v := findTurn(m, turn)
		if v == nil || ev.Seq < v.ToolSeq {
			return nil
		}
		v.ToolSeq = ev.Seq + 1
		if v.Phase == "done" {
			return nil
		}
		room := turnRoom(m, v)
		r := m.Rooms[room]
		if r == nil || r.HideTools {
			return nil
		}
		if v.ToolLines >= toolSummaryLimit {
			if v.ToolLines == toolSummaryLimit {
				v.ToolLines++
				addNoticeFull(m, &Notice{ID: turn + "-tool-more", Room: room, Thread: v.Thread, Text: "… further tool calls are not shown", Plain: true})
			}
			return nil
		}
		if text == "" {
			return nil
		}
		v.ToolLines++
		addNoticeFull(m, &Notice{ID: fmt.Sprintf("%s-tool-%d", turn, ev.Seq), Room: room, Thread: v.Thread, Text: text, Plain: true})
		return nil
	})
}
