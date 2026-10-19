package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const menuOwner = "@admin:chat.test"

func menuCommandTest(t *testing.T, e *Engine, id, room, text string) {
	t.Helper()
	if err := e.s.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: id, Room: room, Sender: menuOwner, Text: text, Kind: "command", Phase: "queued", Created: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.command(context.Background(), "iris", id)
}

func menuPickTest(t *testing.T, e *Engine, id, menuID, key string) {
	t.Helper()
	menu := e.s.View().Members["iris"].Menus[menuID]
	if menu == nil {
		t.Fatalf("menu %s missing", menuID)
	}
	if err := e.s.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: id, Room: menu.Room, Sender: menuOwner, Text: "!menu-choice", MenuID: menuID, Choice: key, Kind: "command", Phase: "queued", Created: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.command(context.Background(), "iris", id)
}

func restoreMenuStore(t *testing.T, e *Engine) {
	t.Helper()
	b, err := os.ReadFile(e.s.path)
	if err != nil {
		t.Fatal(err)
	}
	c := e.c
	c.Data = t.TempDir()
	if err := os.MkdirAll(filepath.Join(c.Data, "controller"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Data, "controller", "state.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	e.s = s
}

func TestMenuReplyModeAndContextDoNotWake(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) { t.Error("local control woke Hermes"); w.WriteHeader(500) })
	room := "!team:chat.test"
	s.Update(func(st *State) error { st.Members["iris"].Rooms[room] = &Room{Mention: true}; return nil })
	menuCommandTest(t, e, "home", room, "!menu")
	menuPickTest(t, e, "more", "home-menu", "5️⃣")
	menuPickTest(t, e, "reply", "more-menu", "1️⃣")
	menuPickTest(t, e, "off", "reply-menu", "2️⃣")
	if s.View().Members["iris"].Rooms[room].Mention {
		t.Fatal("reply mode was not changed")
	}
	menuPickTest(t, e, "stale-on", "reply-menu", "1️⃣")
	if s.View().Members["iris"].Rooms[room].Mention {
		t.Fatal("used menu changed reply mode")
	}
	menuCommandTest(t, e, "dedicated", "!iris:chat.test", "!require_mention on")
	if s.View().Members["iris"].Rooms["!iris:chat.test"].Mention {
		t.Fatal("dedicated room requires a mention")
	}
	menuCommandTest(t, e, "context", room, "!context")
	m := s.View().Members["iris"]
	if !strings.Contains(m.Outbox[len(m.Outbox)-1].Text, "HTTP API does not expose") {
		t.Fatal("context blocker was hidden")
	}
	menuCommandTest(t, e, "another", room, "!help")
	menuPickTest(t, e, "old-navigation", "another-menu", "5️⃣")
	menuCommandTest(t, e, "replacement", room, "!menu")
	menuPickTest(t, e, "stale-navigation", "old-navigation-menu", "1️⃣")
	if !strings.Contains(s.View().Members["iris"].Outbox[len(s.View().Members["iris"].Outbox)-1].Text, "stale") {
		t.Fatal("replaced menu remained usable")
	}
}

func TestMenuConversationsSurviveRestartAndRequireIdle(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) { t.Error("conversation switch woke Hermes") })
	room := "!iris:chat.test"
	oldID := ""
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		old := getSession(m, room, "")
		old.ID, old.Title = "original-session", "Original conversation"
		oldID = old.ID
		getSession(m, room, "$legacy").Title = "Legacy thread"
		m.DefaultModel, m.DefaultProvider = "default-model", "custom:local"
		m.Turns = append(m.Turns, &Turn{ID: "busy", Room: room, Phase: "queued", Text: "pending"})
		return nil
	})
	menuCommandTest(t, e, "blocked", room, "!new")
	if getSession(s.View().Members["iris"], room, "").ID != oldID {
		t.Fatal("switched away from queued work")
	}
	for _, phase := range []string{"running", "held"} {
		s.Update(func(st *State) error {
			findTurn(st.Members["iris"], "busy").Phase = phase
			st.Members["iris"].Outbox = nil
			return nil
		})
		menuCommandTest(t, e, "blocked-"+phase, room, "!new")
		if getSession(s.View().Members["iris"], room, "").ID != oldID {
			t.Fatal("switched away from active/uncertain work")
		}
	}
	s.Update(func(st *State) error {
		findTurn(st.Members["iris"], "busy").Phase = "done"
		st.Members["iris"].Outbox = nil
		return nil
	})
	menuCommandTest(t, e, "new", room, "!new")
	fresh := getSession(s.View().Members["iris"], room, "")
	if fresh.ID == oldID || fresh.Model != "default-model" || fresh.Provider != "custom:local" {
		t.Fatal("new conversation lost default or reused history")
	}
	menuCommandTest(t, e, "resume-list", room, "!resume Original")
	menu := s.View().Members["iris"].Menus["resume-list-menu"]
	if len(menu.Options) != 2 || menu.Options[0].Value != "room" {
		t.Fatal("resume list included threads or ignored filter")
	}
	restoreMenuStore(t, e)
	e.s.Update(func(st *State) error { st.Members["iris"].Outbox = nil; return nil })
	menuPickTest(t, e, "resume", "resume-list-menu", "1️⃣")
	m := e.s.View().Members["iris"]
	if getSession(m, room, "").ID != oldID || len(m.Sessions[room]) != 3 {
		t.Fatal("resume lost history or legacy thread mapping")
	}
	restoreMenuStore(t, e)
	if getSession(e.s.View().Members["iris"], room, "").ID != oldID {
		t.Fatal("selection did not survive restart")
	}
}

func TestMenuModelsScopedAndImmutableRequests(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		default:
			replyJSON(w, map[string]any{"providers": []any{map[string]any{"slug": "custom:local", "models": []string{"model-a", "model-b", "model-c", "model-d", "model-e", "model-f"}}}})
		}
	})
	room, other := "!iris:chat.test", "!other:chat.test"
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Rooms[other] = &Room{Mention: true}
		getSession(m, other, "")
		m.Turns = append(m.Turns, &Turn{ID: "submitted", Room: room, Phase: "running", RunID: "r1", Request: json.RawMessage(`{"model":"immutable"}`)})
		return nil
	})
	menuCommandTest(t, e, "catalog", room, "!models")
	menuPickTest(t, e, "page-two", "catalog-menu", "➡️")
	menuPickTest(t, e, "model", "page-two-menu", "1️⃣")
	menuPickTest(t, e, "scope", "model-menu", "1️⃣")
	m := s.View().Members["iris"]
	if getSession(m, room, "").Model != "model-f" || getSession(m, other, "").Model != "" {
		t.Fatal("chat-scoped model leaked")
	}
	menuCommandTest(t, e, "global-selection", room, "!model model-b custom:local")
	menuPickTest(t, e, "global", "global-selection-menu", "2️⃣")
	m = s.View().Members["iris"]
	if m.DefaultModel != "model-b" || getSession(m, other, "").Provider != "custom:local" || string(findTurn(m, "submitted").Request) != `{"model":"immutable"}` {
		t.Fatal("global scope or immutable submission broken")
	}
	// A resolved choice resumes its stored action after restart, without needing a live menu.
	menuCommandTest(t, e, "restart-selection", room, "!model model-c custom:local")
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: "resolved", Room: room, Sender: menuOwner, Text: "!menu-choice", MenuID: "restart-selection-menu", Choice: "1️⃣", Kind: "command", Phase: "queued"})
		return nil
	})
	if !e.resolveMenu("iris", "resolved") {
		t.Fatal("choice not persisted")
	}
	restoreMenuStore(t, e)
	e.command(context.Background(), "iris", "resolved")
	if getSession(e.s.View().Members["iris"], room, "").Model != "model-c" {
		t.Fatal("resolved action lost after restart")
	}
	before := len(e.s.View().Members["iris"].Outbox)
	e.command(context.Background(), "iris", "resolved")
	if len(e.s.View().Members["iris"].Outbox) != before {
		t.Fatal("completed action replayed")
	}
}

func TestMenuDeliveryRetriesAndReactionSync(t *testing.T) {
	c, s := testStore(t)
	var mu sync.Mutex
	paths := []string{}
	failMessage, failReaction := true, true
	events := []any{}
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sync") {
			var filter map[string]any
			json.Unmarshal([]byte(r.URL.Query().Get("filter")), &filter)
			if !strings.Contains(string(jsonBytes(filter)), "m.reaction") {
				t.Error("reactions excluded from sync")
			}
			replyJSON(w, syncResponse(events, false))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/send/m.room.message/") && failMessage {
			failMessage = false
			w.WriteHeader(500)
			return
		}
		if strings.Contains(r.URL.Path, "/send/m.reaction/") && failReaction {
			failReaction = false
			w.WriteHeader(500)
			return
		}
		replyJSON(w, map[string]string{"event_id": "$menu-event"})
	})
	e := &Engine{c: c, s: s, mx: mx}
	menuCommandTest(t, e, "open", "!iris:chat.test", "!menu")
	for i := 0; i < 4; i++ {
		s.Update(func(st *State) error {
			if len(st.Members["iris"].Outbox) > 0 {
				st.Members["iris"].Outbox[0].Next = time.Time{}
			}
			return nil
		})
		if err := mx.sendNext(context.Background(), s, "iris"); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.View().Members["iris"].Outbox) != 0 || len(paths) != 8 || paths[0] != paths[1] || paths[2] != paths[3] {
		t.Fatalf("unstable menu transactions or missing chips: %v", paths)
	}
	reaction := func(id, sender, target, key string) any {
		return map[string]any{"event_id": id, "sender": sender, "type": "m.reaction", "content": map[string]any{"m.relates_to": map[string]string{"rel_type": "m.annotation", "event_id": target, "key": key}}}
	}
	events = []any{reaction("$stranger", "@other:chat.test", "$menu-event", "5️⃣"), reaction("$bot", "@agent_iris:chat.test", "$menu-event", "5️⃣"), reaction("$unknown", menuOwner, "$ordinary", "5️⃣"), reaction("$pick", menuOwner, "$menu-event", "5️⃣")}
	for i := 0; i < 2; i++ {
		if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
			t.Fatal(err)
		}
	}
	m := s.View().Members["iris"]
	if findTurn(m, "$stranger") != nil || findTurn(m, "$bot") != nil || findTurn(m, "$unknown") != nil || len(m.Turns) != 2 {
		t.Fatal("reaction authorization/deduplication failed")
	}
	e.command(context.Background(), "iris", "$pick")
	if s.View().Members["iris"].Menus["$pick-menu"] == nil {
		t.Fatal("reaction did not navigate")
	}
}

func TestMenuApprovalAutomaticAndExact(t *testing.T) {
	var approvals atomic.Int32
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		case "/v1/runs":
			replyJSON(w, map[string]string{"run_id": "r1"})
		case "/v1/runs/r1/approval":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["choice"] != "once" || body["request_id"] != "exact" {
				t.Error("approval widened")
			}
			approvals.Add(1)
			replyJSON(w, map[string]bool{"ok": true})
		default:
			replyJSON(w, map[string]any{"status": "waiting_for_approval", "approval": map[string]string{"request_id": "exact", "command": "sensitive tool"}})
		}
	})
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: "run", Room: "!iris:chat.test", Sender: menuOwner, Text: "do a thing", Phase: "queued"})
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.work(ctx, "iris", "run") }()
	deadline := time.Now().Add(5 * time.Second)
	for s.View().Members["iris"].Menus["run-approval-exact"] == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	menu := s.View().Members["iris"].Menus["run-approval-exact"]
	if menu == nil || menu.Owner != menuOwner {
		t.Fatal("automatic approval menu missing")
	}
	menuCommandTest(t, e, "home-during-approval", "!iris:chat.test", "!menu")
	menuPickTest(t, e, "approve", "run-approval-exact", "✅")
	menuPickTest(t, e, "duplicate-approve", "run-approval-exact", "✅")
	if approvals.Load() != 1 {
		t.Fatal("approval menu repeated or failed")
	}
}

func TestMenuReactionStopBypassesSlowModelRead(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once, released sync.Once
	defer released.Do(func() { close(release) })
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			once.Do(func() { close(started); <-release })
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		default:
			replyJSON(w, map[string]any{"data": []any{map[string]string{"id": "route"}}})
		}
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		postMenu(m, "safety", m.Room, menuOwner, "Safety", []MenuOption{{Label: "Stop", Action: "stop"}}, false)
		m.Turns = append(m.Turns, &Turn{ID: "slow-models", Room: m.Room, Sender: menuOwner, Kind: "command", Phase: "queued", Text: "!models"}, &Turn{ID: "pending", Room: m.Room, Text: "do not run", Phase: "queued"})
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.tick(ctx, "iris")
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("model read did not start")
	}
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Turns = append(m.Turns, &Turn{ID: "urgent-stop", Room: m.Room, Sender: menuOwner, Kind: "command", Phase: "queued", Text: "!menu-choice", MenuID: "safety", Choice: "1️⃣"})
		return nil
	})
	e.tick(ctx, "iris")
	await(t, func() bool {
		m := s.View().Members["iris"]
		return findTurn(m, "urgent-stop").Phase == "done" && findTurn(m, "pending").Phase == "done"
	})
	released.Do(func() { close(release) })
}

func TestMenuSyncUsesCurrentDeliveryMappingAndMentionExemption(t *testing.T) {
	c, s := testStore(t)
	room := "!team:chat.test"
	s.Update(func(st *State) error {
		st.Members["iris"].Rooms[room] = &Room{Mention: true}
		return nil
	})
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		// Simulate an outbox delivery completed while the long poll was outstanding.
		s.Update(func(st *State) error {
			m := st.Members["iris"]
			postMenu(m, "delivered", room, menuOwner, "Reply mode", []MenuOption{{Label: "Every message", Action: "reply-mode", Value: "off"}}, false)
			m.Menus["delivered"].EventID = "$delivered"
			return nil
		})
		event := map[string]any{"event_id": "$tap", "sender": menuOwner, "type": "m.reaction", "content": map[string]any{"m.relates_to": map[string]string{"rel_type": "m.annotation", "event_id": "$delivered", "key": "1️⃣"}}}
		replyJSON(w, syncRooms(map[string]any{room: map[string]any{"timeline": map[string]any{"events": []any{event, message("$ignored", menuOwner, "not addressed", "")}}}}))
	})
	if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	e := &Engine{c: c, s: s, mx: mx}
	e.command(context.Background(), "iris", "$tap")
	m := s.View().Members["iris"]
	if findTurn(m, "$ignored") != nil || m.Rooms[room].Mention {
		t.Fatal("reaction lost to long-poll snapshot or mention gate")
	}
}

func TestMenuVersionTwoUpgradeAndExpiry(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected API access") })
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		getSession(m, m.Room, "").ID = "preserved-v2-session"
		m.Rooms[m.Room].Mention = true
		return nil
	})
	old := s.View()
	old.Version = 2
	if err := os.WriteFile(s.path, jsonBytes(old), 0600); err != nil {
		t.Fatal(err)
	}
	restoreMenuStore(t, e)
	m := e.s.View().Members["iris"]
	if e.s.View().Version != stateVersion || m.Rooms[m.Room].Mention || getSession(m, m.Room, "").ID != "preserved-v2-session" {
		t.Fatal("v2 upgrade lost session or dedicated-room policy")
	}
	menuCommandTest(t, e, "expiring", m.Room, "!menu")
	e.s.Update(func(st *State) error {
		st.Members["iris"].Menus["expiring-menu"].Created = time.Now().Add(-25 * time.Hour)
		return nil
	})
	menuPickTest(t, e, "expired-tap", "expiring-menu", "5️⃣")
	if e.s.View().Members["iris"].Menus["expired-tap-menu"] != nil {
		t.Fatal("expired menu executed")
	}
}
