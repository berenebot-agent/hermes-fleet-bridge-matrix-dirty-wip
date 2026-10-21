package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// widgetEngine builds an engine whose homeserver answers whoami and records widget installs.
func widgetEngine(t *testing.T, puts *[]map[string]any) (*Engine, *Store) {
	t.Helper()
	c, s := testStore(t)
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/account/whoami":
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			switch token {
			case "owner-token":
				replyJSON(w, map[string]string{"user_id": "@admin:chat.test"})
			case "other-token":
				replyJSON(w, map[string]string{"user_id": "@other:chat.test"})
			default:
				w.WriteHeader(http.StatusForbidden)
			}
		case strings.Contains(r.URL.Path, "/state/im.vector.modular.widgets/"):
			if r.Method == http.MethodPut {
				var content map[string]any
				json.NewDecoder(r.Body).Decode(&content)
				if puts != nil {
					*puts = append(*puts, content)
				}
				replyJSON(w, map[string]string{"event_id": "$widget"})
				return
			}
			replyJSON(w, map[string]string{"type": "m.custom", "url": "https://chat.test/widget/fleet?room=!iris:chat.test#k=old"})
		default:
			replyJSON(w, map[string]string{})
		}
	})
	return &Engine{c: c, s: s, mx: mx, agents: map[string]*agentRuntime{}}, s
}

func widgetRequest(t *testing.T, e *Engine, method, path, token, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("X-Fleet-Token", token)
	}
	if user != "" {
		r.Header.Set("Authorization", "Bearer "+user)
	}
	w := httptest.NewRecorder()
	e.widgetHandler(w, r)
	return w
}

func TestWidgetMenuShowsTheOpenMenu(t *testing.T) {
	e, s := widgetEngine(t, nil)
	if err := s.Update(func(st *State) error {
		st.Members["iris"].Rooms["!iris:chat.test"].WidgetToken = "widget-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	menuCommandTest(t, e, "open", "!iris:chat.test", "!menu")

	w := widgetRequest(t, e, "GET", "/widget/menu", "widget-token", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("menu request failed: %d %s", w.Code, w.Body.String())
	}
	var reply widgetMenuReply
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Menu != "open-menu" || !strings.Contains(reply.Title, "Agent controls") {
		t.Fatalf("unexpected menu reply: %+v", reply)
	}
	if len(reply.Options) == 0 || reply.Options[0].Key != "1️⃣" || reply.Options[0].Label == "" {
		t.Fatalf("menu options not offered as buttons: %+v", reply.Options)
	}
	if w := widgetRequest(t, e, "GET", "/widget/menu", "wrong-token", "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown widget token accepted: %d", w.Code)
	}
}

func TestWidgetSelectRunsTheMenuChoice(t *testing.T) {
	e, s := widgetEngine(t, nil)
	if err := s.Update(func(st *State) error {
		st.Members["iris"].Rooms["!iris:chat.test"].WidgetToken = "widget-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	menuCommandTest(t, e, "open", "!iris:chat.test", "!menu")

	w := widgetRequest(t, e, "POST", "/widget/select", "widget-token", "owner-token", `{"menu":"open-menu","key":"5️⃣"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("press refused: %d %s", w.Code, w.Body.String())
	}
	m := s.View().Members["iris"]
	var turn *Turn
	for _, t := range m.Turns {
		if t.MenuID == "open-menu" {
			turn = t
		}
	}
	if turn == nil {
		t.Fatal("press did not queue a menu turn")
	}
	if turn.Choice != "5️⃣" || turn.Sender != "@admin:chat.test" || turn.Kind != "command" || turn.Phase != "queued" {
		t.Fatalf("unexpected menu turn: %+v", turn)
	}
	// The press runs the same path a reaction tap runs.
	e.command(context.Background(), "iris", turn.ID)
	if s.View().Members["iris"].Menus["open-menu"].Used != true {
		t.Fatal("press did not consume the menu")
	}
}

func TestWidgetSelectRefusesForeignAndClosedMenus(t *testing.T) {
	e, s := widgetEngine(t, nil)
	if err := s.Update(func(st *State) error {
		st.Members["iris"].Rooms["!iris:chat.test"].WidgetToken = "widget-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	menuCommandTest(t, e, "open", "!iris:chat.test", "!menu")

	if w := widgetRequest(t, e, "POST", "/widget/select", "widget-token", "other-token", `{"menu":"open-menu","key":"1️⃣"}`); w.Code != http.StatusConflict {
		t.Fatalf("another member's press accepted: %d", w.Code)
	}
	if w := widgetRequest(t, e, "POST", "/widget/select", "widget-token", "", `{"menu":"open-menu","key":"1️⃣"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("press without an access token accepted: %d", w.Code)
	}
	if w := widgetRequest(t, e, "POST", "/widget/select", "wrong-token", "owner-token", `{"menu":"open-menu","key":"1️⃣"}`); w.Code != http.StatusNotFound {
		t.Fatalf("press with an unknown widget token accepted: %d", w.Code)
	}
	if w := widgetRequest(t, e, "POST", "/widget/select", "widget-token", "owner-token", `{"menu":"open-menu"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("press without a choice accepted: %d", w.Code)
	}
	if err := s.Update(func(st *State) error {
		st.Members["iris"].Menus["open-menu"].Used = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := widgetRequest(t, e, "POST", "/widget/select", "widget-token", "owner-token", `{"menu":"open-menu","key":"1️⃣"}`); w.Code != http.StatusConflict {
		t.Fatalf("press on a closed menu accepted: %d", w.Code)
	}
	m := s.View().Members["iris"]
	for _, turn := range m.Turns {
		if turn.MenuID == "open-menu" {
			t.Fatal("a refused press queued a turn")
		}
	}
}

func TestWidgetInstallPublishesTheRoomWidget(t *testing.T) {
	var puts []map[string]any
	e, s := widgetEngine(t, &puts)
	e.ensureWidget(context.Background(), "iris", "!iris:chat.test")
	if len(puts) != 1 {
		t.Fatalf("widget not published: %v", puts)
	}
	token := s.View().Members["iris"].Rooms["!iris:chat.test"].WidgetToken
	if token == "" {
		t.Fatal("widget token not stored")
	}
	address, _ := puts[0]["url"].(string)
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	if puts[0]["type"] != "m.custom" || parsed.Query().Get("room") != "!iris:chat.test" || parsed.Fragment != "k="+token {
		t.Fatalf("unexpected widget state event: %v", puts[0])
	}
	// The panel is published once per room.
	e.ensureWidget(context.Background(), "iris", "!iris:chat.test")
	if len(puts) != 1 {
		t.Fatalf("widget republished: %v", puts)
	}
}

func TestWidgetPageAndRoutes(t *testing.T) {
	e, _ := widgetEngine(t, nil)
	w := widgetRequest(t, e, "GET", "/widget/fleet", "", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("panel not served: %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("unexpected panel content type: %s", ct)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self'") || !strings.Contains(csp, "frame-ancestors 'self'") {
		t.Fatalf("panel served without a policy: %s", csp)
	}
	if !strings.Contains(w.Body.String(), "/widget/menu") {
		t.Fatal("panel does not read the room menu")
	}
	var ready atomic.Bool
	h := e.widgetRoutes(&ready, http.NotFoundHandler())
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest("GET", "/widget/fleet", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("panel served before the bridge was ready: %d", recorder.Code)
	}
	ready.Store(true)
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest("GET", "/widget/fleet", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("panel refused after the bridge was ready: %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest("GET", "/index.html", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("other paths not passed on: %d", recorder.Code)
	}
}
