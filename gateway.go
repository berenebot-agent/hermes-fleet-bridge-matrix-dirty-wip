package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"time"
)

// A fleet member's Hermes gateway keeps a control socket in its home directory
// (HERMES_HOME). The member's home is bind-mounted into this container at the same
// absolute path, so the bridge can ask the gateway directly instead of guessing from
// container state. The socket answers one JSON request per line.

// gatewayCall sends one request to the agent's gateway control socket.
func gatewayCall(ctx context.Context, home, verb string) (map[string]any, error) {
	if home == "" {
		return nil, errors.New("the agent has no known home directory")
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "unix", filepath.Join(home, "gateway.sock"))
	if err != nil {
		return nil, errors.New("the agent's gateway control socket is unreachable")
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return nil, errors.New("the agent's gateway control socket could not be configured")
	}
	if _, err = conn.Write(append(jsonBytes(map[string]any{"id": 1, "verb": verb}), '\n')); err != nil {
		return nil, errors.New("the agent's gateway control socket rejected the request")
	}
	buf := make([]byte, 64<<10)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, errors.New("the agent's gateway did not answer")
	}
	var reply struct {
		OK     bool           `json:"ok"`
		Error  string         `json:"error"`
		Result map[string]any `json:"result"`
	}
	if json.Unmarshal(buf[:n], &reply) != nil || !reply.OK {
		return nil, errors.New("the agent's gateway refused the request")
	}
	return reply.Result, nil
}

// gatewayPID reads the running gateway's process id from its control socket.
func gatewayPID(ctx context.Context, home string) (float64, error) {
	result, err := gatewayCall(ctx, home, "identify")
	if err != nil {
		return 0, err
	}
	pid, _ := result["pid"].(float64)
	if pid <= 0 {
		return 0, errors.New("the agent's gateway did not report a process id")
	}
	return pid, nil
}

// restartGateway asks the agent's Hermes gateway to drain its work and restart itself.
// This is Hermes' own restart path (the same one its updater and its /restart command
// use): the gateway drains in-flight turns, exits, and the container's restart policy
// brings it back. The bridge only asks, waits for the new process and reports honestly.
func (e *Engine) restartGateway(ctx context.Context, id, room string) string {
	m := e.s.View().Members[id]
	rt := e.runtime(id)
	rt.lifecycle.Lock()
	defer rt.lifecycle.Unlock()
	pid, err := gatewayPID(ctx, m.Home)
	if err != nil {
		return "The agent's gateway is not running, or its control socket is unreachable, so there was nothing to restart. Send a message to wake it."
	}
	e.cancelAgentRuns(ctx, id)
	e.lifecycleNote(id, "restart", "Agent "+agentName(m)+" is restarting…")
	result, err := gatewayCall(ctx, m.Home, "pause-for-update")
	if err != nil {
		return "The agent's gateway refused the restart request: " + err.Error()
	}
	if pausing, _ := result["pausing"].(bool); !pausing {
		return "The agent's gateway is already stopping; no second restart was requested."
	}
	deadline := time.Now().Add(e.c.Start)
	restarted := false
	for time.Now().Before(deadline) {
		if fresh, err := gatewayPID(ctx, m.Home); err == nil && fresh != pid {
			restarted = true
			break
		}
		select {
		case <-ctx.Done():
			return "The restart was requested, but the bridge stopped waiting for the agent to come back."
		case <-time.After(time.Second):
		}
	}
	if !restarted {
		return "The gateway was asked to restart but did not come back within " + e.c.Start.String() + ". Check the agent's container locally."
	}
	for deadline := time.Now().Add(e.c.Start); time.Now().Before(deadline); {
		if _, _, err := e.readyAPI(ctx, m); err == nil {
			e.lifecycleNote(id, "ready", "Agent "+agentName(m)+" is ready.")
			_ = e.s.Update(func(st *State) error {
				st.Members[id].LastActivity = time.Now().UTC()
				return nil
			})
			return "Agent " + agentName(m) + " restarted and is ready again. Conversations, history and pending work are unchanged."
		}
		select {
		case <-ctx.Done():
			return "The restart was requested, but the bridge stopped waiting for the agent to come back."
		case <-time.After(time.Second):
		}
	}
	return "The gateway restarted, but its API did not become ready in time. Check the agent's container locally."
}

// cancelAgentRuns asks Hermes to stop every run this agent has in flight, so a gateway
// restart drains immediately instead of waiting for the turns to finish. Queued work is
// left alone: it survives the restart and is dispatched afterwards.
func (e *Engine) cancelAgentRuns(ctx context.Context, id string) {
	m := e.s.View().Members[id]
	_ = e.s.Update(func(st *State) error {
		for _, v := range st.Members[id].Turns {
			if v.Kind == "" && v.Phase == "running" {
				v.Stop = true
			}
		}
		return nil
	})
	endpoint, err := e.docker.endpoint(ctx, m)
	if err != nil {
		return
	}
	for _, v := range m.Turns {
		if v.Kind == "" && v.Phase == "running" && v.RunID != "" {
			_ = api(ctx, "POST", endpoint, "/v1/runs/"+url.PathEscape(v.RunID)+"/stop", e.mx.key(id), map[string]any{}, nil, nil)
		}
	}
}
