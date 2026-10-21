package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGatewaySocket serves the Hermes control-socket protocol for one agent home.
// The pid changes after a restart request, the way a restarted gateway reports a new process.
func fakeGatewaySocket(t *testing.T, dir string, restarted *bool, mu *sync.Mutex) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(dir, "gateway.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				n, err := c.Read(buf)
				if err != nil {
					return
				}
				var req struct {
					ID   any    `json:"id"`
					Verb string `json:"verb"`
				}
				json.Unmarshal(buf[:n], &req)
				result := map[string]any{}
				switch req.Verb {
				case "identify":
					mu.Lock()
					pid := 1.0
					if *restarted {
						pid = 2
					}
					mu.Unlock()
					result = map[string]any{"pid": pid, "kind": "hermes-gateway"}
				case "pause-for-update":
					mu.Lock()
					*restarted = true
					mu.Unlock()
					result = map[string]any{"pausing": true, "already_stopping": false}
				}
				out, _ := json.Marshal(map[string]any{"ok": true, "protocol": 1, "result": result, "id": req.ID})
				c.Write(append(out, '\n'))
			}(conn)
		}
	}()
}

func lastReply(t *testing.T, e *Engine) string {
	t.Helper()
	m := e.s.View().Members["iris"]
	if len(m.Outbox) == 0 {
		t.Fatal("no reply was queued")
	}
	return m.Outbox[len(m.Outbox)-1].Text
}

func TestRestartCommandRequiresAdministrator(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) { t.Error("restart reached Hermes") })
	room := "!iris:chat.test"
	if err := s.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: "restart", Room: room, Sender: "@someone:chat.test", Text: "!restart", Kind: "command", Phase: "queued", Created: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.command(context.Background(), "iris", "restart")
	if reply := lastReply(t, e); !strings.Contains(reply, "administrator") {
		t.Fatalf("non-administrator restart was not refused: %q", reply)
	}
}

func TestRestartCommandRestartsTheGateway(t *testing.T) {
	var mu sync.Mutex
	restarted := false
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		default:
			t.Errorf("unexpected API call %s", r.URL.Path)
		}
	})
	home := t.TempDir()
	fakeGatewaySocket(t, home, &restarted, &mu)
	s.Update(func(st *State) error {
		st.Members["iris"].Home = home
		return nil
	})
	room := "!iris:chat.test"
	menuCommandTest(t, e, "restart", room, "!restart")
	mu.Lock()
	asked := restarted
	mu.Unlock()
	if !asked {
		t.Fatal("the gateway was never asked to restart")
	}
	if reply := lastReply(t, e); !strings.Contains(reply, "restarted and is ready again") {
		t.Fatalf("restart reply was not confirmed: %q", reply)
	}
}

func TestRestartCommandRefusesWhenTheGatewayIsNotRunning(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) { t.Error("restart reached Hermes") })
	s.Update(func(st *State) error {
		st.Members["iris"].Home = t.TempDir()
		return nil
	})
	menuCommandTest(t, e, "restart", "!iris:chat.test", "!restart")
	if reply := lastReply(t, e); !strings.Contains(reply, "nothing to restart") {
		t.Fatalf("a stopped gateway was not reported: %q", reply)
	}
}

func TestSteerCommandReachesTheRunningTurn(t *testing.T) {
	var mu sync.Mutex
	var steered string
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/steer") {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			steered = r.URL.Path + " " + body["input"]
			mu.Unlock()
			replyJSON(w, map[string]any{"accepted": true})
			return
		}
		t.Errorf("unexpected API call %s", r.URL.Path)
	})
	room := "!iris:chat.test"
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Turns = append(m.Turns, &Turn{ID: "run", Room: room, Sender: menuOwner, Text: "hello", Phase: "running", RunID: "run-1"})
		return nil
	})
	menuCommandTest(t, e, "steer", room, "!steer focus on the tests")
	mu.Lock()
	got := steered
	mu.Unlock()
	if !strings.Contains(got, "/v1/runs/run-1/steer") || !strings.Contains(got, "focus on the tests") {
		t.Fatalf("steer was not delivered to the running turn: %q", got)
	}
	if reply := lastReply(t, e); !strings.Contains(reply, "delivered") {
		t.Fatalf("steer reply missing: %q", reply)
	}
}

func TestRetryCommandResubmitsTheLastPrompt(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/capabilities":
			fakeCapabilities(w)
		case r.URL.Path == "/health/detailed":
			fakeHealth(w)
		case r.URL.Path == "/api/sessions/s1/messages":
			replyJSON(w, map[string]any{"object": "list", "data": []any{
				map[string]any{"role": "user", "content": "do the thing"},
				map[string]any{"role": "assistant", "content": "done"},
			}})
		default:
			t.Errorf("unexpected API call %s", r.URL.Path)
		}
	})
	room := "!iris:chat.test"
	s.Update(func(st *State) error {
		getSession(st.Members["iris"], room, "").ID = "s1"
		return nil
	})
	menuCommandTest(t, e, "retry", room, "!retry")
	m := s.View().Members["iris"]
	found := ""
	for _, v := range m.Turns {
		if v.Kind == "" && v.Text == "do the thing" && v.Phase == "queued" {
			found = v.ID
		}
	}
	if found == "" {
		t.Fatal("the retry was not queued")
	}
	if !strings.Contains(lastReply(t, e), "Re-submitted the last prompt") {
		t.Fatalf("retry reply missing: %q", lastReply(t, e))
	}
}

func TestVersionAndStatusReportTheLiveAgent(t *testing.T) {
	e, _ := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			replyJSON(w, map[string]any{"status": "ok", "version": "0.21.6", "gateway_state": "running", "active_agents": 0, "gateway_busy": false, "gateway_drainable": true, "platforms": map[string]any{"api_server": map[string]any{"state": "connected"}}, "metrics_today": map[string]any{"requests": 3, "messages": 2, "tokens": 1000}, "readiness": map[string]any{"status": "ok", "checks": map[string]any{"background_queues": map[string]int{"active_api_runs": 0, "process_completions": 0, "active_delegations": 0}}}})
		default:
			t.Errorf("unexpected API call %s", r.URL.Path)
		}
	})
	room := "!iris:chat.test"
	menuCommandTest(t, e, "version", room, "!version")
	if reply := lastReply(t, e); !strings.Contains(reply, "0.21.6") {
		t.Fatalf("version reply missing: %q", reply)
	}
	menuCommandTest(t, e, "status", room, "!status")
	if reply := lastReply(t, e); !strings.Contains(reply, "0.21.6") || !strings.Contains(reply, "api_server connected") {
		t.Fatalf("status does not carry the live agent state: %q", reply)
	}
}

func TestContextCommandReportsWhatTheAPIExposes(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/capabilities":
			fakeCapabilities(w)
		case r.URL.Path == "/health/detailed":
			fakeHealth(w)
		case strings.HasPrefix(r.URL.Path, "/api/sessions/"):
			replyJSON(w, map[string]any{"object": "hermes.session", "session": map[string]any{"id": "s1", "message_count": 4, "input_tokens": 1200, "output_tokens": 30}})
		default:
			t.Errorf("unexpected API call %s", r.URL.Path)
		}
	})
	room := "!iris:chat.test"
	s.Update(func(st *State) error {
		getSession(st.Members["iris"], room, "").ID = "s1"
		return nil
	})
	menuCommandTest(t, e, "context", room, "!context")
	reply := lastReply(t, e)
	if !strings.Contains(reply, "input_tokens") || !strings.Contains(reply, "1200") {
		t.Fatalf("context report does not carry the API counters: %q", reply)
	}
	if !strings.Contains(reply, "not available") {
		t.Fatalf("context report does not state the limit: %q", reply)
	}
}
