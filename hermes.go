package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Engine struct {
	c        Config
	s        *Store
	mx       *Matrix
	docker   *Docker
	agents   map[string]*agentRuntime
	wg       sync.WaitGroup
	mediaMu  sync.Mutex
	agentsMu sync.Mutex
	fleetMu  sync.Mutex
	// widgetMu guards widgets, the rooms whose widget panel is already published.
	widgetMu sync.Mutex
	widgets  map[string]bool
}
type agentRuntime struct {
	mu          sync.Mutex
	lifecycle   sync.Mutex
	busy        map[string]bool
	commands    bool
	controls    map[string]bool
	lastMediaGC time.Time
	started     bool
	// stopNotice tracks the idle-shutdown announcement: 0 none, 1 "shutting down" sent,
	// 2 the failed shutdown reported. It keeps a failing stop from repeating the message
	// on every retry, and is guarded by lifecycle.
	stopNotice int
}
type runStatus struct {
	ID       string `json:"run_id"`
	Session  string `json:"session_id"`
	Status   string `json:"status"`
	Output   string `json:"output"`
	Approval struct {
		ID      string `json:"request_id"`
		Command string `json:"command"`
		Reason  string `json:"reason"`
	} `json:"approval"`
}
type capabilities struct {
	Features struct {
		Idempotency struct {
			Supported, Durable bool
			Retention          int `json:"retention_seconds"`
		} `json:"runs_idempotency"`
	} `json:"features"`
}
type detailedHealth struct {
	Status       string `json:"status"`
	Version      string `json:"version"`
	Busy         *bool  `json:"gateway_busy"`
	Drainable    *bool  `json:"gateway_drainable"`
	Active       *int   `json:"active_agents"`
	GatewayState string `json:"gateway_state"`
	Platforms    map[string]struct {
		State string `json:"state"`
	} `json:"platforms"`
	MetricsToday struct {
		Requests int `json:"requests"`
		Messages int `json:"messages"`
		Tokens   int `json:"tokens"`
	} `json:"metrics_today"`
	Readiness struct {
		Status string `json:"status"`
		Checks struct {
			Background struct {
				Runs      *int `json:"active_api_runs"`
				Processes *int `json:"process_completions"`
				Delegates *int `json:"active_delegations"`
			} `json:"background_queues"`
			Disk struct {
				UsedPercent float64 `json:"used_percent"`
			} `json:"disk"`
		} `json:"checks"`
	} `json:"readiness"`
}

func terminal(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled" || status == "interrupted"
}
func (e *Engine) readyAPI(ctx context.Context, m *Member) (string, int, error) {
	endpoint, err := e.docker.endpoint(ctx, m)
	if err != nil {
		return "", 0, err
	}
	var cap capabilities
	if err = api(ctx, "GET", endpoint, "/v1/capabilities", e.mx.key(m.ID), nil, &cap, nil); err != nil {
		return "", 0, err
	}
	if !cap.Features.Idempotency.Supported || !cap.Features.Idempotency.Durable || cap.Features.Idempotency.Retention < 300 {
		return "", 0, errors.New("Hermes requires durable run idempotency with an advertised recovery window")
	}
	var health detailedHealth
	if err = api(ctx, "GET", endpoint, "/health/detailed", e.mx.key(m.ID), nil, &health, nil); err != nil {
		return "", 0, err
	}
	if health.Readiness.Status != "ok" {
		return "", 0, errors.New("Hermes authenticated readiness is degraded")
	}
	bg := health.Readiness.Checks.Background
	if health.Busy == nil || health.Drainable == nil || health.Active == nil || bg.Runs == nil || bg.Processes == nil || bg.Delegates == nil {
		return "", 0, errors.New("Hermes health must expose complete authenticated activity counters")
	}
	return endpoint, cap.Features.Idempotency.Retention, nil
}

// lifecycleNote queues a container-lifecycle message in the agent's own room. The reply outbox
// already delivers notices durably, in order and with retries, so no extra Matrix path is
// needed. The notice ID carries a timestamp because every lifecycle event is distinct.
// Container state is sent as an m.notice so a client shows it without notifying: the user
// asked not to be interrupted by start/ready/stop. A failed shutdown stays an ordinary
// message because it reports a problem the user has to act on.
func (e *Engine) lifecycleNote(id, kind, text string) {
	_ = e.s.Update(func(st *State) error {
		m := st.Members[id]
		if m == nil || m.Room == "" {
			return nil
		}
		addNoticeFull(m, &Notice{ID: id + "-" + kind + "-" + fmt.Sprintf("%d", time.Now().UnixNano()), Room: m.Room, Text: text, Silent: kind != "stop-failed"})
		return nil
	})
}
func (e *Engine) ensure(ctx context.Context, id, room string) (string, int, error) {
	rt := e.runtime(id)
	rt.lifecycle.Lock()
	defer rt.lifecycle.Unlock()
	m := e.s.View().Members[id]
	if m.Paused || m.Deleted {
		return "", 0, errors.New("agent is paused or deleted")
	}
	endpoint, retention, err := e.readyAPI(ctx, m)
	if err == nil {
		// The agent is up, so the next shutdown is a new cycle and may announce itself.
		rt.stopNotice = 0
		return endpoint, retention, nil
	}
	if !m.Enabled {
		return "", 0, errors.New("removed agent unavailable; it will not be restarted")
	}
	startCtx, cancel := context.WithTimeout(ctx, e.c.Start)
	defer cancel()
	if err = e.docker.checkProject(startCtx, m); err != nil {
		return "", 0, err
	}
	e.lifecycleNote(id, "start", "Agent "+agentName(m)+" is starting…")
	started := false
	if _, inspectErr := e.docker.endpoint(startCtx, m); inspectErr != nil {
		if !errors.Is(inspectErr, errStopped) {
			return "", 0, inspectErr
		}
		if _, err = e.docker.compose(startCtx, m, "up", "-d", "--no-build", "--pull", "never"); err != nil {
			return "", 0, err
		}
		started = true
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastTyping := time.Time{}
	for {
		if time.Since(lastTyping) > 15*time.Second {
			e.mx.typing(startCtx, m, room, true)
			lastTyping = time.Now()
		}
		endpoint, retention, err = e.readyAPI(startCtx, m)
		if err == nil {
			e.lifecycleNote(id, "ready", "Agent "+agentName(m)+" is ready.")
			return endpoint, retention, nil
		}
		if !started {
			if _, inspectErr := e.docker.endpoint(startCtx, m); errors.Is(inspectErr, errStopped) {
				if _, err = e.docker.compose(startCtx, m, "up", "-d", "--no-build", "--pull", "never"); err != nil {
					return "", 0, err
				}
				started = true
			} else if inspectErr != nil {
				return "", 0, inspectErr
			}
		}
		select {
		case <-startCtx.Done():
			return "", 0, errors.New("Hermes startup/readiness timed out; inspect its configuration locally")
		case <-ticker.C:
		}
	}
}
func (e *Engine) fail(id, turn, message string, held bool) {
	_ = e.s.Update(func(st *State) error {
		m := st.Members[id]
		t := findTurn(m, turn)
		if t == nil || t.Phase == "done" {
			return nil
		}
		if held {
			t.Phase = "held"
		} else {
			t.Phase = "done"
		}
		m.LastActivity = time.Now().UTC()
		addNotice(m, t.ID+"-failure", turnRoom(m, t), t.Thread, message+" Turn ID: "+t.ID)
		return nil
	})
}

func (e *Engine) apiUnavailableMessage(ctx context.Context, m *Member) string {
	message := "Cannot reach a ready, durably idempotent Hermes API."
	if _, err := e.docker.endpoint(ctx, m); errors.Is(err, errStopped) {
		message = "The agent container is not running."
	} else if err == nil {
		message = "The agent container is running, but its Hermes API did not become ready. Check its dependency environment and API configuration locally."
	}
	return message + " No automatic replay; use !discard only after confirming inactivity."
}

func (e *Engine) work(ctx context.Context, id, turn string) {
	defer func() { rt := e.runtime(id); rt.mu.Lock(); delete(rt.busy, turn); rt.mu.Unlock() }()
	m := e.s.View().Members[id]
	t := findTurn(m, turn)
	if t == nil || t.Phase == "done" || t.Phase == "held" || t.Phase == "steering" || t.Kind != "" {
		return
	}
	if t.Media != nil && t.Phase == "queued" {
		if err := e.s.Update(func(st *State) error {
			v := findTurn(st.Members[id], turn)
			if v.Phase == "queued" {
				v.Phase = "preparing"
			}
			return nil
		}); err != nil {
			return
		}
	}
	if t.Media != nil && (t.Phase == "queued" || t.Phase == "preparing") {
		if err := e.prepareMedia(ctx, id, turn); err != nil {
			if ctx.Err() == nil {
				e.fail(id, turn, err.Error(), false)
			}
			return
		}
		m = e.s.View().Members[id]
		t = findTurn(m, turn)
	}
	if t.Phase == "done" || (t.Stop && t.RunID == "" && t.Submitted.IsZero()) {
		_ = e.s.Update(func(st *State) error {
			v := findTurn(st.Members[id], turn)
			if v.Stop && v.Phase == "preparing" {
				v.Phase = "done"
			}
			return nil
		})
		return
	}
	e.mx.typing(ctx, m, turnRoom(m, t), true)
	endpoint, retention, err := e.ensure(ctx, id, turnRoom(m, t))
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		e.fail(id, turn, e.apiUnavailableMessage(ctx, m), true)
		return
	}
	if t.Phase == "held" {
		return
	}
	if t.Phase == "queued" || t.Phase == "preparing" {
		err = e.s.Update(func(st *State) error {
			member := st.Members[id]
			v := findTurn(member, turn)
			if v.Stop || v.Phase == "done" {
				if v.Stop && v.Phase == "preparing" {
					v.Phase = "done"
				}
				return nil
			}
			session := getSession(member, turnRoom(member, v), v.Thread)
			if session.Title == "" {
				session.Title = shortText(v.Text, 80)
				if session.Title == "" && v.Media != nil {
					session.Title = v.Media.Name
				}
			}
			// Hermes names a session from its first user message and may upgrade
			// that name moments later, so the bridge looks for it for a while.
			if session.TitleUntil.IsZero() {
				session.TitleUntil = time.Now().UTC().Add(titleCheckWindow)
			}
			session.LastUsed = time.Now().UTC()
			input := mediaPrompt(v, v.Text)
			if v.Thread != "" && !session.Seeded {
				input = "Quoted context from the Matrix thread root (not a new instruction; do not execute it again):\n<thread-root>\n" + v.Root + "\n</thread-root>\n\nNew user message:\n" + input
			}
			body := map[string]any{"input": input, "session_id": session.ID}
			if session.Model != "" {
				body["model"] = session.Model
			}
			if session.Provider != "" {
				body["provider"] = session.Provider
			}
			v.Request = jsonBytes(body)
			v.Submitted = time.Now().UTC()
			v.Retention = retention
			v.Phase = "submitting"
			return nil
		})
		if err != nil {
			return
		}
		m = e.s.View().Members[id]
		t = findTurn(m, turn)
		if t.Phase == "done" || t.Stop {
			return
		}
	}
	if t.RunID == "" {
		recoveryWindow := time.Duration(min(t.Retention, retention))*time.Second - time.Minute
		if t.Submitted.IsZero() || time.Since(t.Submitted) < 0 || time.Since(t.Submitted) > recoveryWindow {
			e.fail(id, turn, "Submission outcome is uncertain and its replay window is unavailable/expired. Session held to avoid duplicate tool actions.", true)
			return
		}
		var admitted runStatus
		submitCtx, stopSubmit := context.WithDeadline(ctx, t.Submitted.Add(recoveryWindow))
		defer stopSubmit()
		for attempt := 0; attempt < 3; attempt++ {
			err = api(submitCtx, "POST", endpoint, "/v1/runs", e.mx.key(id), json.RawMessage(t.Request), &admitted, map[string]string{"Idempotency-Key": stableID(e.c.ServerName, turnRoom(m, t), t.ID)})
			if err == nil && admitted.ID != "" {
				break
			}
			if ctx.Err() != nil {
				return
			}
			if httpStatus(err, 400) || httpStatus(err, 401) || httpStatus(err, 403) || httpStatus(err, 409) {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
		}
		if err != nil || admitted.ID == "" {
			e.fail(id, turn, "Hermes admission could not be reconciled safely. Session held; do not blindly resend a possibly executed instruction.", true)
			return
		}
		if e.s.Update(func(st *State) error {
			v := findTurn(st.Members[id], turn)
			v.RunID = admitted.ID
			v.Phase = "running"
			return nil
		}) != nil {
			return
		}
	}
	// Mirror the run's tool calls into the room as one summary line each. The
	// stream is advisory: if it cannot be opened, the turn still finishes.
	m = e.s.View().Members[id]
	t = findTurn(m, turn)
	toolCtx, stopTools := context.WithCancel(ctx)
	defer stopTools()
	toolsDone := e.attachToolStream(toolCtx, id, turn, endpoint, t.RunID, t.ToolSeq)
	failures := 0
	lastTyping := time.Time{}
	for ctx.Err() == nil {
		m = e.s.View().Members[id]
		t = findTurn(m, turn)
		if t == nil {
			return
		}
		if t.RunID == "" {
			return
		}
		if t.Stop {
			_ = api(ctx, "POST", endpoint, "/v1/runs/"+url.PathEscape(t.RunID)+"/stop", e.mx.key(id), map[string]any{}, nil, nil)
		}
		var r runStatus
		err = api(ctx, "GET", endpoint, "/v1/runs/"+url.PathEscape(t.RunID), e.mx.key(id), nil, &r, nil)
		if err != nil {
			failures++
			if failures >= 8 {
				e.fail(id, turn, "Lost authenticated run visibility. Session held and idle shutdown disabled until the original run is reconciled.", true)
				return
			}
			if fresh, _, re := e.readyAPI(ctx, m); re == nil {
				endpoint = fresh
			}
		} else {
			failures = 0
			if terminal(r.Status) {
				if r.Status == "interrupted" {
					e.fail(id, turn, "Hermes restarted during this turn; earlier tool actions may have happened. Session held; confirm inactivity then !discard and explicitly resend if needed.", true)
					return
				}
				text := r.Output
				var attachments []*Attachment
				if r.Status == "completed" {
					var mediaErr error
					text, attachments, mediaErr = e.outputMedia(m, t, text)
					if mediaErr != nil {
						text += "\n\nAttachment delivery refused: " + mediaErr.Error()
					}
				}
				if r.Status == "failed" {
					text = "Hermes reported a failed run. Some tool actions may already have happened; inspect the agent locally before retrying."
				}
				if r.Status == "cancelled" {
					text = "Hermes confirmed the run stopped. Completed tool actions were not undone."
				}
				// The tool stream ends with the run, so waiting for it lets the last
				// summary lines reach the room before the answer does.
				select {
				case <-toolsDone:
				case <-time.After(toolSummaryWait):
				}
				_ = e.s.Update(func(st *State) error {
					member := st.Members[id]
					v := findTurn(member, turn)
					v.Phase = "done"
					if r.Status == "completed" {
						session := getSession(member, turnRoom(member, v), v.Thread)
						session.Seeded = true
						if r.Session != "" && len(r.Session) <= 256 {
							session.ID = r.Session
						}
					}
					member.LastActivity = time.Now().UTC()
					if text != "" || len(attachments) == 0 {
						addNotice(member, v.ID+"-reply", turnRoom(member, v), v.Thread, e.redact(text))
					}
					for i, a := range attachments {
						member.Outbox = append(member.Outbox, &Notice{ID: fmt.Sprintf("%s-file-%d", v.ID, i), Room: turnRoom(member, v), Thread: v.Thread, TurnID: v.ID, Media: a})
					}
					return nil
				})
				// The session's own name is now worth reading: Hermes wrote it with
				// this turn's first message, or will upgrade it moments from now.
				e.refreshRoomName(ctx, id, turnRoom(m, t), endpoint)
				return
			}
			if r.Status == "waiting_for_approval" && r.Approval.ID != "" && t.Approval != r.Approval.ID {
				detail := r.Approval.Command
				if detail == "" {
					detail = r.Approval.Reason
				}
				if len(detail) > 2000 {
					detail = detail[:2000]
				}
				notice := "Tool approval required for request " + r.Approval.ID + ":\n" + e.redact(detail) + "\n\n!approve " + r.Approval.ID + "\n!deny " + r.Approval.ID + "\nOnly this exact request can be approved once."
				_ = e.s.Update(func(st *State) error {
					member := st.Members[id]
					v := findTurn(member, turn)
					v.Approval = r.Approval.ID
					if v.Thread == "" {
						menuID := v.ID + "-approval-" + r.Approval.ID
						postMenu(member, menuID, turnRoom(member, v), v.Sender, notice, []MenuOption{{Key: "✅", Label: "Approve once", Action: "approve", Value: r.Approval.ID}, {Key: "❌", Label: "Deny", Action: "deny", Value: r.Approval.ID}}, false)
						if menu := member.Menus[menuID]; menu != nil {
							menu.ApprovalTurn = v.ID
						}
					} else {
						addNotice(member, v.ID+"-approval-"+r.Approval.ID, turnRoom(member, v), v.Thread, notice)
					}
					return nil
				})
			}
		}
		if time.Since(lastTyping) > 15*time.Second {
			e.mx.typing(ctx, m, turnRoom(m, t), true)
			lastTyping = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// steerTarget finds the running turn of one conversation that a follow-up can be steered
// into. Only a running turn with a known run ID accepts steer input; a held or otherwise
// uncertain turn never does.
func steerTarget(m *Member, conversation, thread string) *Turn {
	for _, v := range m.Turns {
		if v.Kind == "" && turnRoom(m, v) == conversation && v.Thread == thread && v.RunID != "" && v.Phase == "running" {
			return v
		}
	}
	return nil
}

// steer injects a follow-up message into the running turn of its own conversation, the
// way a Telegram follow-up steers the live turn: the running turn answers both messages
// instead of the follow-up waiting behind it. A refused steer falls back to the queue, so
// the message is never lost.
func (e *Engine) steer(ctx context.Context, id, turn string) {
	defer func() { rt := e.runtime(id); rt.mu.Lock(); delete(rt.busy, turn); rt.mu.Unlock() }()
	m := e.s.View().Members[id]
	t := findTurn(m, turn)
	if t == nil || t.Kind != "" || t.Phase != "queued" || t.Stop {
		return
	}
	conversation := turnRoom(m, t)
	target := steerTarget(m, conversation, t.Thread)
	if target == nil {
		return
	}
	// Record the attempt before anything else: a turn already offered to a steer is
	// never offered again, whatever the outcome.
	if e.s.Update(func(st *State) error {
		v := findTurn(st.Members[id], turn)
		if v == nil {
			return errors.New("the follow-up turn is gone")
		}
		v.SteerRunID = target.RunID
		if v.Media != nil && v.Phase == "queued" {
			v.Phase = "preparing"
		}
		return nil
	}) != nil {
		return
	}
	input := t.Text
	if t.Media != nil {
		// The attachment has to be staged, and a voice note transcribed, before its
		// content can be steered into the running turn.
		if err := e.prepareMedia(ctx, id, turn); err != nil {
			e.steerQueued(id, turn, "The attachment could not be prepared, so your message is queued and runs next.")
			return
		}
		m = e.s.View().Members[id]
		t = findTurn(m, turn)
		if t == nil || t.Phase == "done" || t.Stop {
			return
		}
		input = mediaAttachment(t, t.Text)
	}
	if strings.TrimSpace(input) == "" {
		e.steerQueued(id, turn, "Your message carried no text to steer, so it is queued and runs next.")
		return
	}
	// Persist the intent before the remote call: a crash after the steer lands must not
	// replay the message as a second turn.
	if e.s.Update(func(st *State) error {
		v := findTurn(st.Members[id], turn)
		if v == nil {
			return errors.New("the follow-up turn is gone")
		}
		if v.Phase == "done" {
			return nil
		}
		v.Phase = "steering"
		v.SteerRunID = target.RunID
		return nil
	}) != nil {
		return
	}
	endpoint, err := e.docker.endpoint(ctx, m)
	if err != nil {
		e.steerQueued(id, turn, "The agent's API is unreachable, so your message is queued and runs next.")
		return
	}
	if api(ctx, "POST", endpoint, "/v1/runs/"+url.PathEscape(target.RunID)+"/steer", e.mx.key(id), map[string]string{"input": input}, nil, nil) != nil {
		e.steerQueued(id, turn, "The running turn did not accept your message, so it is queued and runs next.")
		return
	}
	_ = e.s.Update(func(st *State) error {
		member := st.Members[id]
		v := findTurn(member, turn)
		if v.Phase != "steering" {
			return nil
		}
		v.Phase = "done"
		member.LastActivity = time.Now().UTC()
		addNotice(member, turn+"-steered", turnRoom(member, v), v.Thread, "Steered into the running turn of this conversation; the agent answers both messages together.")
		return nil
	})
}

// steerQueued returns a refused steer to the queue: the message is answered as its own
// turn once the running turn finishes.
func (e *Engine) steerQueued(id, turn, notice string) {
	_ = e.s.Update(func(st *State) error {
		member := st.Members[id]
		v := findTurn(member, turn)
		if v == nil || v.Phase == "done" || v.Phase == "held" {
			return nil
		}
		v.Phase = "queued"
		addNotice(member, turn+"-steer-queued", turnRoom(member, v), v.Thread, notice)
		return nil
	})
}

// holdSteer holds a follow-up whose steer outcome is unknown: the message may have
// reached the running turn before the bridge restarted, so it is held rather than
// repeated as a second turn.
func (e *Engine) holdSteer(id, turn string) {
	_ = e.s.Update(func(st *State) error {
		member := st.Members[id]
		v := findTurn(member, turn)
		if v == nil || v.Phase != "steering" {
			return nil
		}
		v.Phase = "held"
		addNotice(member, turn+"-steer-held", turnRoom(member, v), v.Thread, "The bridge restarted while steering your message into the running turn, so it is not known whether the agent received it. It was held rather than repeated: confirm inactivity, then !discard "+turn+" to release it, or resend the message.")
		return nil
	})
}

func (e *Engine) redact(s string) string {
	e.mx.mu.Lock()
	defer e.mx.mu.Unlock()
	secrets := []string{e.c.Password, e.mx.secrets.Registration}
	for _, a := range e.mx.secrets.Accounts {
		secrets = append(secrets, a.Password, a.Token)
	}
	for _, key := range e.mx.secrets.APIKeys {
		secrets = append(secrets, key)
	}
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

// sessionUsage reads the provider-reported cumulative counters of one Hermes session.
func (e *Engine) sessionUsage(ctx context.Context, endpoint, id, sessionID string) (map[string]any, error) {
	var usage map[string]any
	if api(ctx, "GET", endpoint, "/api/sessions/"+url.PathEscape(sessionID), e.mx.key(id), nil, &usage, nil) != nil {
		return nil, errors.New("session usage unavailable")
	}
	if nested, ok := usage["session"].(map[string]any); ok {
		usage = nested
	}
	safe := map[string]any{}
	for _, k := range []string{"message_count", "tool_call_count", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"} {
		if v, ok := usage[k]; ok {
			safe[k] = v
		}
	}
	return safe, nil
}

// lastUserPrompt reads the last user message Hermes recorded for a session. The bridge
// clears finished turn text, so the agent's own record is the source for a retry.
func (e *Engine) lastUserPrompt(ctx context.Context, endpoint, id, sessionID string) (string, error) {
	var page struct {
		Data []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"data"`
	}
	if api(ctx, "GET", endpoint, "/api/sessions/"+url.PathEscape(sessionID)+"/messages?limit=50&order=latest", e.mx.key(id), nil, &page, nil) != nil {
		return "", errors.New("session messages unavailable")
	}
	for i := len(page.Data) - 1; i >= 0; i-- {
		if page.Data[i].Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(page.Data[i].Content, &text) == nil {
			return strings.TrimSpace(text), nil
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(page.Data[i].Content, &parts) == nil {
			var b strings.Builder
			for _, p := range parts {
				b.WriteString(p.Text)
			}
			return strings.TrimSpace(b.String()), nil
		}
	}
	return "", nil
}

// liveAgentState reads the agent's own view of itself — version, gateway state,
// connected platforms and today's counters. It never wakes the agent and stays
// silent when the agent is not running.
func (e *Engine) liveAgentState(ctx context.Context, id string) string {
	m := e.s.View().Members[id]
	endpoint, err := e.docker.endpoint(ctx, m)
	if err != nil {
		return ""
	}
	var health detailedHealth
	if api(ctx, "GET", endpoint, "/health/detailed", e.mx.key(id), nil, &health, nil) != nil {
		return ""
	}
	state := ""
	if health.Version != "" {
		state = "Hermes " + health.Version
	}
	if health.GatewayState != "" {
		if state != "" {
			state += ", "
		}
		state += "gateway " + health.GatewayState
	}
	names := make([]string, 0, len(health.Platforms))
	for name := range health.Platforms {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		state += ", " + name + " " + health.Platforms[name].State
	}
	state += fmt.Sprintf("; today: %d requests, %d messages, %d tokens", health.MetricsToday.Requests, health.MetricsToday.Messages, health.MetricsToday.Tokens)
	return state + ".\n"
}
func (e *Engine) tick(ctx context.Context, id string) {
	if ctx.Err() != nil {
		return
	}
	rt := e.runtime(id)
	m := e.s.View().Members[id]
	if m.Deleted {
		return
	}
	// Every room the agent serves carries a widget panel that renders its menus. The install
	// is a room-lifecycle step, so it happens here rather than inside a command.
	for room := range m.Rooms {
		e.ensureWidget(ctx, id, room)
		// The visible room name carries the session's own name, which Hermes writes
		// after the first turn and may upgrade a moment later.
		e.ensureSessionName(ctx, id, room)
	}
	rt.mu.Lock()
	if rt.controls == nil {
		rt.controls = map[string]bool{}
	}
	// A steer interrupted by a restart has an unknown outcome: the message may have
	// reached the running turn. Hold it instead of repeating it as a second turn.
	for _, t := range m.Turns {
		if t.Phase == "steering" && !rt.busy[t.ID] {
			e.holdSteer(id, t.ID)
		}
	}
	for _, t := range m.Turns {
		// The controller also dispatches the plain reply that answers a name prompt.
		plain := m.Controller && t.Kind == ""
		if t.Kind != "command" && !plain {
			continue
		}
		if (t.Phase != "queued" && t.Phase != "processing") || rt.controls[t.ID] {
			continue
		}
		urgent := plain
		fields := strings.Fields(t.Text)
		if len(fields) > 0 {
			switch fields[0] {
			case "!menu", "!help", "!status", "!stop", "!approve", "!deny", "!discard", "!resend", "!restart", "!steer", "!retry", "!version", "!context":
				urgent = true
			}
		}
		if t.MenuID != "" {
			option := t.Selection
			if option == nil {
				if menu := m.Menus[t.MenuID]; menu != nil {
					for i := range menu.Options {
						if menu.Options[i].Key == t.Choice {
							option = &menu.Options[i]
							break
						}
					}
				}
			}
			if option != nil {
				switch option.Action {
				case "status", "stop", "approve", "deny":
					urgent = true
				}
			}
		}
		if (!urgent && rt.commands) || len(rt.controls) >= 5 {
			continue
		}
		rt.controls[t.ID] = true
		if !urgent {
			rt.commands = true
		}
		e.wg.Add(1)
		go func(turn string, urgent bool) {
			defer e.wg.Done()
			if m.Controller {
				e.fleetCommand(ctx, turn)
			} else {
				e.command(ctx, id, turn)
			}
			rt.mu.Lock()
			delete(rt.controls, turn)
			if !urgent {
				rt.commands = false
			}
			rt.mu.Unlock()
		}(t.ID, urgent)
	}
	if m.Controller {
		rt.mu.Unlock()
		return
	}
	sessions := map[string]bool{}
	controls := map[string]bool{}
	for _, t := range m.Turns {
		if t.Kind == "command" && (t.Phase == "queued" || t.Phase == "processing") {
			controls[sessionKey(turnRoom(m, t), t.Thread)] = true
		}
		if activePhase(t.Phase) || rt.busy[t.ID] {
			sessions[sessionKey(turnRoom(m, t), t.Thread)] = true
		}
	}
	for _, t := range m.Turns {
		if t.Kind == "command" || t.Phase == "done" || t.Phase == "held" || rt.busy[t.ID] {
			continue
		}
		if len(rt.busy) >= e.c.Parallel {
			break
		}
		key := sessionKey(turnRoom(m, t), t.Thread)
		if t.Phase == "queued" && (!m.Enabled || sessions[key] || controls[key]) {
			// A plain follow-up that arrives while its conversation is busy is steered
			// into the running turn instead of waiting behind it. A command, a turn
			// already offered to a steer, or a conversation with no running turn keeps
			// the message in the queue.
			if m.Enabled && t.Kind == "" && t.SteerRunID == "" && sessions[key] && steerTarget(m, turnRoom(m, t), t.Thread) != nil {
				rt.busy[t.ID] = true
				e.wg.Add(1)
				go func(turn string) { defer e.wg.Done(); e.steer(ctx, id, turn) }(t.ID)
			}
			continue
		}
		if t.Phase != "queued" && t.Phase != "preparing" && t.Phase != "submitting" && t.Phase != "running" {
			continue
		}
		rt.busy[t.ID] = true
		sessions[key] = true
		e.wg.Add(1)
		go func(turn string) { defer e.wg.Done(); e.work(ctx, id, turn) }(t.ID)
	}
	rt.mu.Unlock()
}
func (e *Engine) idle(ctx context.Context, id string) {
	rt := e.runtime(id)
	if !rt.lifecycle.TryLock() {
		return
	}
	defer rt.lifecycle.Unlock()
	if !rt.mu.TryLock() {
		return
	}
	m := e.s.View().Members[id]
	busy := len(rt.busy) > 0 || len(rt.controls) > 0 || rt.commands
	rt.mu.Unlock()
	if !m.Enabled || m.Controller || m.Deleted || m.Draining || busy || pending(m) > 0 || time.Since(m.LastActivity) < e.c.Idle {
		return
	}
	endpoint, err := e.docker.endpoint(ctx, m)
	if err != nil {
		return
	}
	var health detailedHealth
	if api(ctx, "GET", endpoint, "/health/detailed", e.mx.key(id), nil, &health, nil) != nil {
		return
	}
	bg := health.Readiness.Checks.Background
	if health.Busy == nil || *health.Busy || health.Drainable == nil || !*health.Drainable || health.Active == nil || *health.Active != 0 || bg.Runs == nil || *bg.Runs != 0 || bg.Processes == nil || *bg.Processes != 0 || bg.Delegates == nil || *bg.Delegates != 0 {
		return
	}
	current := e.s.View().Members[id]
	if pending(current) > 0 || time.Since(current.LastActivity) < e.c.Idle {
		return
	}
	if e.docker.checkProject(ctx, m) != nil {
		return
	}
	downCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if rt.stopNotice == 0 {
		rt.stopNotice = 1
		e.lifecycleNote(id, "stop", "Agent "+agentName(m)+" is shutting down (idle timeout).")
	}
	if _, err := e.docker.compose(downCtx, m, "down", "--timeout", "60"); err != nil {
		if rt.stopNotice == 1 {
			rt.stopNotice = 2
			e.lifecycleNote(id, "stop-failed", "Agent "+agentName(m)+" is still running: shutdown failed. Inspect the member configuration locally.")
		}
		return
	}
	rt.stopNotice = 0
}
func (e *Engine) Start(ctx context.Context) {
	members := e.s.View().Members
	for id, m := range members {
		if m.Room != "" && !m.Deleted {
			e.startMember(ctx, id)
		}
	}
	if e.c.FleetDir != "" {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					e.fleetStep(ctx)
				}
			}
		}()
	}
}

func (e *Engine) runtime(id string) *agentRuntime {
	e.agentsMu.Lock()
	defer e.agentsMu.Unlock()
	if e.agents == nil {
		e.agents = map[string]*agentRuntime{}
	}
	if e.agents[id] == nil {
		e.agents[id] = &agentRuntime{busy: map[string]bool{}}
	}
	return e.agents[id]
}

func (e *Engine) startMember(ctx context.Context, id string) {
	rt := e.runtime(id)
	rt.mu.Lock()
	if rt.started {
		rt.mu.Unlock()
		return
	}
	rt.started = true
	rt.mu.Unlock()
	e.wg.Add(4)
	go func() {
		defer e.wg.Done()
		failures := 0
		// Invitations are reconciled once per start: an incrementing sync only offers an
		// invite relative to its cursor, so this is the only chance to see a pending one.
		if !e.s.View().Members[id].Controller {
			_ = e.mx.reconcileInvites(ctx, e.s, id)
		}
		for ctx.Err() == nil {
			if e.s.View().Members[id].Deleted {
				return
			}
			if err := e.mx.syncOnce(ctx, e.s, id); err != nil {
				failures++
				if httpStatus(err, 401) && !e.s.View().Members[id].Deleted {
					_, _ = e.mx.account(ctx, "agent_"+id, false)
				}
				if failures >= 3 && !e.s.View().Members[id].SyncFailed {
					_ = e.s.Update(func(st *State) error { st.Members[id].SyncFailed = true; return nil })
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			} else {
				failures = 0
				if e.s.View().Members[id].SyncFailed {
					_ = e.s.Update(func(st *State) error { st.Members[id].SyncFailed = false; return nil })
				}
			}
		}
	}()
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if e.s.View().Members[id].Deleted {
					return
				}
				e.tick(ctx, id)
			}
		}
	}()
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if e.s.View().Members[id].Deleted {
					return
				}
				_ = e.mx.sendNext(ctx, e.s, id)
			}
		}
	}()
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m := e.s.View().Members[id]
				if m.Deleted {
					return
				}
				if m.Controller || m.Draining {
					continue
				}
				e.idle(ctx, id)
				rt := e.runtime(id)
				if time.Since(rt.lastMediaGC) > time.Minute {
					e.cleanMedia(id)
					rt.lastMediaGC = time.Now()
				}
			}
		}
	}()
}

const helpText = `Hermes fleet commands (send as plain text):
!menu / !help — tappable, single-use reaction menus
!status — bridge queue/run status and live agent state, no wake
!version — the agent's Hermes version
!models [filter] — paginated model/provider picker (wakes agent)
!model <model> [provider] — choose This chat / Global scope in a reaction menu
!new / !reset — fresh conversation; preserve previous history, requires idle room
!resume [filter] / !conversations — list bridge conversations by first prompt
!context — what the Hermes API exposes about this conversation's context (wakes agent)
!usage — verified cumulative session usage (wakes agent); not context occupancy
!stop — stop this conversation's run and cancel its queue
!steer <text> — inject guidance into this conversation's running turn
!retry — re-submit this conversation's last prompt as a new turn
!approve <request-id> / !deny <request-id> — this conversation's exact approval, once
!discard <turn-id> — release an uncertain turn only after confirmed inactivity; resend explicitly if desired
!resend — retry blocked Matrix replies (a visible duplicate remains possible)
!require_mention on|off — reply mode in additional rooms; dedicated room always responds
!tools on|off — show or hide the agent's tool-call summary lines in this room
!restart — administrator only: restart this agent's Hermes gateway

Use the main room: new thread prompts are unsupported. New/Resume changes future Hermes history, not the visible Matrix history. Global model scope affects only this agent's bridge-managed chats/default, not Hermes global settings. Tools/filesystem are shared. No E2EE: the server can read messages.`

func (e *Engine) command(ctx context.Context, id, turn string) {
	m := e.s.View().Members[id]
	t := findTurn(m, turn)
	if t == nil || t.Phase == "done" {
		return
	}
	if t.MenuID != "" && !e.resolveMenu(id, turn) {
		return
	}
	if e.menuCommand(ctx, id, turn) {
		return
	}
	m = e.s.View().Members[id]
	t = findTurn(m, turn)
	conversation := turnRoom(m, t)
	fields := strings.Fields(t.Text)
	reply := "Unknown command. Send !help; nothing was dispatched."
	if len(fields) == 0 {
		return
	}
	// Persist intent before controls with remote effects; exact request/run IDs make safe retries narrow.
	if e.s.Update(func(st *State) error { findTurn(st.Members[id], turn).Phase = "processing"; return nil }) != nil {
		return
	}
	switch fields[0] {
	case "!help":
		reply = helpText
	case "!status":
		var lines []string
		for _, v := range m.Turns {
			if v.Kind == "" && v.Phase != "done" {
				lines = append(lines, fmt.Sprintf("%s: %s (turn %s)", conversationLabel(m, turnRoom(m, v), v.Thread), v.Phase, v.ID))
			}
		}
		reply = fmt.Sprintf("Agent %s; rooms served: %d; dispatch enabled: %t; pending: %d; reply outbox: %d.\n", id, len(m.Rooms), m.Enabled, pending(m), len(m.Outbox)) + strings.Join(lines, "\n")
		if live := e.liveAgentState(ctx, id); live != "" {
			reply += live
		}
		if len(reply) > 12000 {
			reply = reply[:12000]
		}
	case "!version":
		endpoint, err := e.docker.endpoint(ctx, m)
		if err != nil {
			reply = "The agent is not running, so its Hermes version is unknown. Send a message to wake it."
			break
		}
		var health detailedHealth
		if api(ctx, "GET", endpoint, "/health/detailed", e.mx.key(id), nil, &health, nil) != nil || health.Version == "" {
			reply = "The agent's Hermes version is unavailable."
			break
		}
		reply = "Hermes " + health.Version + " (agent " + agentName(m) + ")."
	case "!steer":
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t.Text), "!steer"))
		if text == "" {
			reply = "Usage: !steer <text> — inject guidance into this conversation's running turn."
			break
		}
		target := steerTarget(m, conversation, t.Thread)
		if target == nil {
			reply = "No running turn in this conversation can be steered."
			break
		}
		endpoint, err := e.docker.endpoint(ctx, m)
		if err != nil {
			reply = "Cannot reach the agent's Hermes API; nothing was steered."
			break
		}
		if api(ctx, "POST", endpoint, "/v1/runs/"+url.PathEscape(target.RunID)+"/steer", e.mx.key(id), map[string]string{"input": text}, nil, nil) != nil {
			reply = "The running turn did not accept the steer instruction."
			break
		}
		reply = "Steering instruction delivered to the running turn of this conversation."
	case "!retry":
		endpoint, _, err := e.ensure(ctx, id, conversation)
		if err != nil {
			reply = "Cannot reach a ready Hermes API, so the last prompt of this conversation is unknown."
			break
		}
		{
			session := m.Sessions[conversation][selectedKey(m, conversation)]
			if session == nil {
				reply = "This conversation has no Hermes session yet."
				break
			}
			prompt, err := e.lastUserPrompt(ctx, endpoint, id, session.ID)
			if err != nil || prompt == "" {
				reply = "This conversation has no recorded prompt to retry."
				break
			}
			retryID := session.ID + "-retry-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			_ = e.s.Update(func(st *State) error {
				member := st.Members[id]
				member.Turns = append(member.Turns, &Turn{ID: retryID, Room: conversation, Thread: t.Thread, Sender: t.Sender, Text: prompt, Phase: "queued", Created: time.Now().UTC()})
				return nil
			})
			reply = "Re-submitted the last prompt of this conversation as a new turn (" + retryID + "). Tool effects from the earlier attempt are not undone."
		}
	case "!restart":
		if t.Sender != "@"+e.c.Admin+":"+e.c.ServerName {
			reply = "Only the administrator can restart an agent's gateway."
			break
		}
		if m.Paused || m.Draining || m.Deleted || !m.Enabled {
			reply = "This agent is paused, draining or unavailable; nothing was restarted."
			break
		}
		if e.s.View().FleetOperation != nil {
			reply = "A fleet operation is in progress; wait for it to finish before restarting the gateway."
			break
		}
		reply = e.restartGateway(ctx, id, conversation)
	case "!context":
		endpoint, _, err := e.ensure(ctx, id, conversation)
		if err != nil {
			reply = "Cannot reach a ready Hermes API, so this conversation's counters are unavailable."
			break
		}
		{
			session := m.Sessions[conversation][selectedKey(m, conversation)]
			if session == nil {
				reply = "This conversation has no Hermes session yet."
				break
			}
			safe, err := e.sessionUsage(ctx, endpoint, id, session.ID)
			if err != nil {
				reply = "Session counters unavailable (the session may not have run yet)."
				break
			}
			reply = "Live context occupancy and headroom are not available here: Hermes computes them inside the gateway from the live agent context, and no supported surface (the HTTP API or the gateway's control socket) exposes them. What the API does expose for this conversation, provider-reported and cumulative:\n" + string(jsonBytes(safe)) + "\nThese are cumulative counters, not current context-window occupancy."
		}
	case "!stop":
		_ = e.s.Update(func(st *State) error {
			member := st.Members[id]
			for _, v := range member.Turns {
				if v.Kind != "" || turnRoom(member, v) != conversation || v.Thread != t.Thread {
					continue
				}
				if v.Phase == "queued" || v.Phase == "steering" || (v.Phase == "preparing" && v.RunID == "" && v.Submitted.IsZero()) {
					v.Phase = "done"
				} else if activePhase(v.Phase) {
					v.Stop = true
				}
			}
			return nil
		})
		if endpoint, err := e.docker.endpoint(ctx, m); err == nil {
			for _, v := range m.Turns {
				if v.Kind == "" && turnRoom(m, v) == conversation && v.Thread == t.Thread && activePhase(v.Phase) && v.RunID != "" {
					_ = api(ctx, "POST", endpoint, "/v1/runs/"+url.PathEscape(v.RunID)+"/stop", e.mx.key(id), map[string]any{}, nil, nil)
				}
			}
		}
		reply = "Cancellation requested for this conversation; queued turns were cancelled. Active/uncertain work is not confirmed stopped until Hermes reports it. Other conversations are unaffected."
	case "!model":
		reply = "Usage: !model <model> [provider]. Use !models to list options."
	case "!usage":
		endpoint, _, err := e.ensure(ctx, id, conversation)
		if err != nil {
			reply = "Cannot reach a ready Hermes API. Check the agent configuration locally."
			break
		}
		{
			session := m.Sessions[conversation][selectedKey(m, conversation)]
			if session == nil {
				reply = "This conversation has no Hermes session yet."
				break
			}
			safe, err := e.sessionUsage(ctx, endpoint, id, session.ID)
			if err != nil {
				reply = "Session usage unavailable (the session may not have run yet)."
				break
			}
			reply = "Verified cumulative session usage:\n" + string(jsonBytes(safe)) + "\nThese are cumulative counters, not current context-window occupancy; exact remaining context is unavailable."
		}
	case "!approve", "!deny":
		reply = "Usage: !approve <exact-request-id> or !deny <exact-request-id>, in its originating conversation."
		if len(fields) != 2 {
			break
		}
		var target *Turn
		for _, v := range m.Turns {
			if turnRoom(m, v) == conversation && v.Thread == t.Thread && v.RunID != "" && v.Approval == fields[1] && v.Phase == "running" && !v.Stop {
				target = v
				break
			}
		}
		if target == nil {
			reply = "No matching active approval in this conversation. Stale or ambiguous approvals are refused."
			break
		}
		endpoint, err := e.docker.endpoint(ctx, m)
		if err != nil {
			reply = "Cannot verify the active approval; nothing approved."
			break
		}
		var status runStatus
		if api(ctx, "GET", endpoint, "/v1/runs/"+url.PathEscape(target.RunID), e.mx.key(id), nil, &status, nil) != nil || status.Status != "waiting_for_approval" || status.Approval.ID != fields[1] {
			reply = "Approval is no longer current; nothing approved."
			break
		}
		choice := "once"
		if fields[0] == "!deny" {
			choice = "deny"
		}
		err = api(ctx, "POST", endpoint, "/v1/runs/"+url.PathEscape(target.RunID)+"/approval", e.mx.key(id), map[string]string{"choice": choice, "request_id": fields[1]}, nil, nil)
		if err != nil {
			reply = "Approval outcome not confirmed. Check !status; no broader approval was attempted."
		} else {
			reply = "Hermes confirmed this exact approval response: " + choice
		}
	case "!discard":
		reply = "Usage: !discard <held-turn-id>. This acknowledges that earlier tool actions may have happened."
		if len(fields) != 2 {
			break
		}
		target := findTurn(m, fields[1])
		if target == nil || target.Phase != "held" || turnRoom(m, target) != conversation || target.Thread != t.Thread {
			reply = "No matching held turn in this conversation."
			break
		}
		endpoint, _, err := e.readyAPI(ctx, m)
		if err != nil {
			reply = "Cannot prove inactivity through the authenticated API; held turn retained."
			break
		}
		inactive := false
		if target.RunID != "" {
			var status runStatus
			err = api(ctx, "GET", endpoint, "/v1/runs/"+url.PathEscape(target.RunID), e.mx.key(id), nil, &status, nil)
			inactive = err == nil && terminal(status.Status)
		}
		if target.RunID == "" || httpStatus(err, 404) {
			var health detailedHealth
			if api(ctx, "GET", endpoint, "/health/detailed", e.mx.key(id), nil, &health, nil) == nil {
				bg := health.Readiness.Checks.Background
				inactive = bg.Runs != nil && *bg.Runs == 0 && bg.Processes != nil && *bg.Processes == 0 && bg.Delegates != nil && *bg.Delegates == 0 && health.Busy != nil && !*health.Busy
			}
		}
		if !inactive {
			reply = "Original execution is still active or cannot be verified; held turn retained."
			break
		}
		_ = e.s.Update(func(st *State) error { findTurn(st.Members[id], target.ID).Phase = "done"; return nil })
		reply = "Held turn discarded after confirmed inactivity. Prior tool actions were not undone. Send a new message explicitly if you want another attempt."
	case "!resend":
		_ = e.s.Update(func(st *State) error {
			for _, n := range st.Members[id].Outbox {
				n.Attempts = 0
				n.Next = time.Time{}
			}
			return nil
		})
		reply = "Matrix reply retries re-enabled with the original transaction IDs. A visible duplicate is still possible if the server has expired its deduplication history."
	}
	_ = e.s.Update(func(st *State) error {
		member := st.Members[id]
		findTurn(member, turn).Phase = "done"
		member.LastActivity = time.Now().UTC()
		addNotice(member, turn+"-command", turnRoom(member, t), t.Thread, e.redact(reply))
		return nil
	})
}
