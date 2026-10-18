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
	c      Config
	s      *Store
	mx     *Matrix
	docker *Docker
	agents map[string]*agentRuntime
	wg     sync.WaitGroup
}
type agentRuntime struct {
	mu        sync.Mutex
	lifecycle sync.Mutex
	busy      map[string]bool
	commands  bool
	controls  map[string]bool
}
type runStatus struct {
	ID       string `json:"run_id"`
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
func (e *Engine) ensure(ctx context.Context, id string) (string, int, error) {
	rt := e.agents[id]
	rt.lifecycle.Lock()
	defer rt.lifecycle.Unlock()
	m := e.s.View().Members[id]
	endpoint, retention, err := e.readyAPI(ctx, m)
	if err == nil {
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
			e.mx.typing(startCtx, m, true)
			lastTyping = time.Now()
		}
		endpoint, retention, err = e.readyAPI(startCtx, m)
		if err == nil {
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
		addNotice(m, t.ID+"-failure", t.Thread, message+" Turn ID: "+t.ID)
		return nil
	})
}
func (e *Engine) work(ctx context.Context, id, turn string) {
	defer func() { rt := e.agents[id]; rt.mu.Lock(); delete(rt.busy, turn); rt.mu.Unlock() }()
	m := e.s.View().Members[id]
	t := findTurn(m, turn)
	if t == nil || t.Phase == "done" || t.Phase == "held" || t.Kind != "" {
		return
	}
	e.mx.typing(ctx, m, true)
	endpoint, retention, err := e.ensure(ctx, id)
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
				return nil
			}
			session := getSession(member, v.Thread)
			input := v.Text
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
			err = api(submitCtx, "POST", endpoint, "/v1/runs", e.mx.key(id), json.RawMessage(t.Request), &admitted, map[string]string{"Idempotency-Key": stableID(e.c.ServerName, m.Room, t.ID)})
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
						getSession(member, v.Thread).Seeded = true
					}
					member.LastActivity = time.Now().UTC()
					addNotice(member, v.ID+"-reply", v.Thread, e.redact(text))
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
					addNotice(member, v.ID+"-approval-"+r.Approval.ID, v.Thread, notice)
					return nil
				})
			}
		}
		if time.Since(lastTyping) > 15*time.Second {
			e.mx.typing(ctx, m, true)
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
	rt := e.agents[id]
	m := e.s.View().Members[id]
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
			case "!help", "!status", "!stop", "!approve", "!deny", "!discard", "!resend":
				urgent = true
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
			e.command(ctx, id, turn)
			rt.mu.Lock()
			delete(rt.controls, turn)
			if !urgent {
				rt.commands = false
			}
			rt.mu.Unlock()
		}(t.ID, urgent)
	}
	sessions := map[string]bool{}
	controls := map[string]bool{}
	for _, t := range m.Turns {
		if t.Kind == "command" && (t.Phase == "queued" || t.Phase == "processing") {
			controls[sessionKey(t.Thread)] = true
		}
		if activePhase(t.Phase) || rt.busy[t.ID] {
			sessions[sessionKey(t.Thread)] = true
		}
	}
	for _, t := range m.Turns {
		if t.Kind == "command" || t.Phase == "done" || t.Phase == "held" || rt.busy[t.ID] {
			continue
		}
		if len(rt.busy) >= e.c.Parallel {
			break
		}
		if t.Phase == "queued" && (!m.Enabled || sessions[sessionKey(t.Thread)] || controls[sessionKey(t.Thread)]) {
			continue
		}
		if t.Phase != "queued" && t.Phase != "preparing" && t.Phase != "submitting" && t.Phase != "running" {
			continue
		}
		rt.busy[t.ID] = true
		sessions[sessionKey(t.Thread)] = true
		e.wg.Add(1)
		go func(turn string) { defer e.wg.Done(); e.work(ctx, id, turn) }(t.ID)
	}
	rt.mu.Unlock()
}
func (e *Engine) idle(ctx context.Context, id string) {
	rt := e.agents[id]
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
	if !m.Enabled || busy || pending(m) > 0 || time.Since(m.LastActivity) < e.c.Idle {
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
	_, _ = e.docker.compose(downCtx, m, "down", "--timeout", "60")
}
func (e *Engine) Start(ctx context.Context) {
	members := e.s.View().Members
	for id, m := range members {
		if m.Room != "" {
			e.agents[id] = &agentRuntime{busy: map[string]bool{}}
		}
	}
	for id, m := range members {
		if m.Room == "" {
			continue
		}
		id := id
		e.wg.Add(4)
		go func() {
			defer e.wg.Done()
			failures := 0
			for ctx.Err() == nil {
				if err := e.mx.syncOnce(ctx, e.s, id); err != nil {
					failures++
					if httpStatus(err, 401) {
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
					e.idle(ctx, id)
				}
			}
		}()
	}
}

const helpText = `Hermes fleet commands (send as plain text):
!help — this list
!status — bridge queue/run status, no wake
!models — available models (wakes agent)
!model <model> [provider] — select for not-yet-started turns in this conversation
!context — verified session usage (wakes agent); not live window occupancy
!stop — stop this conversation's run and cancel its queue
!approve <request-id> / !deny <request-id> — this conversation's exact approval, once
!discard <turn-id> — release an uncertain turn only after confirmed inactivity; resend explicitly if desired
!resend — retry blocked Matrix replies (a visible duplicate remains possible)

Threads have independent histories and may run in parallel. Tools/filesystem are shared. No E2EE: the server can read messages.`

func (e *Engine) command(ctx context.Context, id, turn string) {
	m := e.s.View().Members[id]
	t := findTurn(m, turn)
	if t == nil {
		return
	}
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
				lines = append(lines, fmt.Sprintf("%s: %s (turn %s)", sessionKey(v.Thread), v.Phase, v.ID))
			}
		}
		reply = fmt.Sprintf("Agent %s; dispatch enabled: %t; pending: %d; reply outbox: %d.\n", id, m.Enabled, pending(m), len(m.Outbox)) + strings.Join(lines, "\n")
		if len(reply) > 12000 {
			reply = reply[:12000]
		}
	case "!stop":
		_ = e.s.Update(func(st *State) error {
			member := st.Members[id]
			for _, v := range member.Turns {
				if v.Kind != "" || v.Thread != t.Thread {
					continue
				}
				if v.Phase == "queued" {
					v.Phase = "done"
				} else if activePhase(v.Phase) {
					v.Stop = true
				}
			}
			return nil
		})
		if endpoint, err := e.docker.endpoint(ctx, m); err == nil {
			for _, v := range m.Turns {
				if v.Kind == "" && v.Thread == t.Thread && activePhase(v.Phase) && v.RunID != "" {
					_ = api(ctx, "POST", endpoint, "/v1/runs/"+url.PathEscape(v.RunID)+"/stop", e.mx.key(id), map[string]any{}, nil, nil)
				}
			}
		}
		reply = "Cancellation requested for this conversation; queued turns were cancelled. Active/uncertain work is not confirmed stopped until Hermes reports it. Other threads are unaffected."
	case "!model":
		if len(fields) != 2 && len(fields) != 3 {
			reply = "Usage: !model <model> [provider]. Use !models to list options."
		} else if len(fields[1]) > 256 || (len(fields) == 3 && len(fields[2]) > 80) {
			reply = "Model identifier too long."
		} else {
			_ = e.s.Update(func(st *State) error {
				session := getSession(st.Members[id], t.Thread)
				session.Model = fields[1]
				session.Provider = ""
				if len(fields) == 3 {
					session.Provider = fields[2]
				}
				return nil
			})
			reply = "Model selection saved for not-yet-started turns in this conversation. Hermes validates availability when the next run is submitted."
		}
	case "!models", "!context":
		endpoint, _, err := e.ensure(ctx, id)
		if err != nil {
			reply = "Cannot reach a ready Hermes API. Check the agent configuration locally."
			break
		}
		if fields[0] == "!models" {
			var catalog struct {
				Providers []struct {
					Slug   string   `json:"slug"`
					Models []string `json:"models"`
				} `json:"providers"`
			}
			var names []string
			if api(ctx, "GET", endpoint, "/api/model/options?include_unconfigured=false", e.mx.key(id), nil, &catalog, nil) == nil {
				for _, provider := range catalog.Providers {
					for _, model := range provider.Models {
						line := model
						if provider.Slug != "" {
							line += "  [provider: " + provider.Slug + "]"
						}
						names = append(names, line)
						if len(names) >= 100 {
							break
						}
					}
					if len(names) >= 100 {
						break
					}
				}
			}
			var models struct {
				Data []struct {
					ID string `json:"id"`
				}
			}
			if len(names) > 0 {
				reply = "Configured model inventory (up to 100):\n" + strings.Join(names, "\n") + "\nUse !model <model> [provider]."
			} else if api(ctx, "GET", endpoint, "/v1/models", e.mx.key(id), nil, &models, nil) != nil {
				reply = "Model listing unavailable on this Hermes API."
			} else {
				for _, model := range models.Data {
					names = append(names, model.ID)
				}
				reply = "Advertised Hermes model/route IDs:\n" + strings.Join(names, "\n") + "\nUse !model <id>; this list is not a claim that every provider catalog model is configured."
			}
		} else {
			session := m.Sessions[sessionKey(t.Thread)]
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
			if v.Thread == t.Thread && v.RunID != "" && v.Approval == fields[1] && v.Phase == "running" && !v.Stop {
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
		if target == nil || target.Phase != "held" || target.Thread != t.Thread {
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
		addNotice(member, turn+"-command", t.Thread, e.redact(reply))
		return nil
	})
}
