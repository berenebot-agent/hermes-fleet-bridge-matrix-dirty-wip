package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	Status    string `json:"status"`
	Busy      *bool  `json:"gateway_busy"`
	Drainable *bool  `json:"gateway_drainable"`
	Active    *int   `json:"active_agents"`
	Readiness struct {
		Status string `json:"status"`
		Checks struct {
			Background struct {
				Runs      *int `json:"active_api_runs"`
				Processes *int `json:"process_completions"`
				Delegates *int `json:"active_delegations"`
			} `json:"background_queues"`
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
func (e *Engine) lifecycleNote(id, kind, text string) {
	_ = e.s.Update(func(st *State) error {
		m := st.Members[id]
		if m == nil || m.Room == "" {
			return nil
		}
		addNotice(m, id+"-"+kind+"-"+fmt.Sprintf("%d", time.Now().UnixNano()), m.Room, "", text)
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
func (e *Engine) work(ctx context.Context, id, turn string) {
	defer func() { rt := e.runtime(id); rt.mu.Lock(); delete(rt.busy, turn); rt.mu.Unlock() }()
	m := e.s.View().Members[id]
	t := findTurn(m, turn)
	if t == nil || t.Phase == "done" || t.Phase == "held" || t.Kind != "" {
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
		e.fail(id, turn, "Cannot reach a ready, durably idempotent Hermes API. No automatic replay; check the agent configuration and use !discard after confirming inactivity.", true)
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
func (e *Engine) tick(ctx context.Context, id string) {
	if ctx.Err() != nil {
		return
	}
	rt := e.runtime(id)
	m := e.s.View().Members[id]
	if m.Deleted {
		return
	}
	rt.mu.Lock()
	if rt.controls == nil {
		rt.controls = map[string]bool{}
	}
	for _, t := range m.Turns {
		if t.Kind != "command" || (t.Phase != "queued" && t.Phase != "processing") || rt.controls[t.ID] {
			continue
		}
		urgent := false
		fields := strings.Fields(t.Text)
		if len(fields) > 0 {
			switch fields[0] {
			case "!menu", "!help", "!status", "!stop", "!approve", "!deny", "!discard", "!resend":
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
		if t.Phase == "queued" && (!m.Enabled || sessions[sessionKey(turnRoom(m, t), t.Thread)] || controls[sessionKey(turnRoom(m, t), t.Thread)]) {
			continue
		}
		if t.Phase != "queued" && t.Phase != "preparing" && t.Phase != "submitting" && t.Phase != "running" {
			continue
		}
		rt.busy[t.ID] = true
		sessions[sessionKey(turnRoom(m, t), t.Thread)] = true
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
!status — bridge queue/run status, no wake
!models [filter] — paginated model/provider picker (wakes agent)
!model <model> [provider] — choose This chat / Global scope in a reaction menu
!new / !reset — fresh conversation; preserve previous history, requires idle room
!resume [filter] / !conversations — list bridge conversations by first prompt
!context — explains the real-context API blocker, no wake
!usage — verified cumulative session usage (wakes agent); not context occupancy
!stop — stop this conversation's run and cancel its queue
!approve <request-id> / !deny <request-id> — this conversation's exact approval, once
!discard <turn-id> — release an uncertain turn only after confirmed inactivity; resend explicitly if desired
!resend — retry blocked Matrix replies (a visible duplicate remains possible)
!require_mention on|off — reply mode in additional rooms; dedicated room always responds

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
		if len(reply) > 12000 {
			reply = reply[:12000]
		}
	case "!stop":
		_ = e.s.Update(func(st *State) error {
			member := st.Members[id]
			for _, v := range member.Turns {
				if v.Kind != "" || turnRoom(member, v) != conversation || v.Thread != t.Thread {
					continue
				}
				if v.Phase == "queued" || (v.Phase == "preparing" && v.RunID == "" && v.Submitted.IsZero()) {
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
			var usage map[string]any
			if api(ctx, "GET", endpoint, "/api/sessions/"+url.PathEscape(session.ID), e.mx.key(id), nil, &usage, nil) != nil {
				reply = "Session usage unavailable (the session may not have run yet)."
			} else {
				if nested, ok := usage["session"].(map[string]any); ok {
					usage = nested
				}
				safe := map[string]any{}
				for _, k := range []string{"message_count", "tool_call_count", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"} {
					if v, ok := usage[k]; ok {
						safe[k] = v
					}
				}
				reply = "Verified cumulative session usage:\n" + string(jsonBytes(safe)) + "\nThese are cumulative counters, not current context-window occupancy; exact remaining context is unavailable."
			}
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
