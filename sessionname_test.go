package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// namedEngine builds an engine whose Hermes API and Matrix homeserver are both observable,
// so a test can watch the room name the bridge writes. It returns the Hermes base URL.
func namedEngine(t *testing.T, hermes http.HandlerFunc) (*Engine, *Store, *[]string, *sync.Mutex, string, *atomic.Value) {
	t.Helper()
	c, s := testStore(t)
	hermesServer := httptest.NewServer(hermes)
	t.Cleanup(hermesServer.Close)
	u, _ := url.Parse(hermesServer.URL)
	port, _ := strconv.Atoi(u.Port())
	s.Update(func(st *State) error { st.Members["iris"].Port = port; return nil })
	dockerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		labels := map[string]string{"com.docker.compose.project": "iris", "com.docker.compose.service": "hermes", "com.docker.compose.project.working_dir": s.View().Members["iris"].Dir}
		if strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/container") {
			replyJSON(w, map[string]any{"Config": map[string]any{"Labels": labels}, "HostConfig": map[string]string{"NetworkMode": "host"}})
		} else {
			replyJSON(w, []any{map[string]any{"Id": "container", "Labels": labels}})
		}
	}))
	t.Cleanup(dockerServer.Close)
	du, _ := url.Parse(dockerServer.URL)
	d := &Docker{client: &http.Client{Transport: rewriteTransport{du, http.DefaultTransport}}}
	names := &[]string{}
	lock := &sync.Mutex{}
	roomName := &atomic.Value{}
	roomName.Store("")
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/state/m.room.name/") && r.Method == "PUT" {
			var body struct {
				Name string `json:"name"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			lock.Lock()
			*names = append(*names, body.Name)
			lock.Unlock()
			roomName.Store(body.Name)
			replyJSON(w, map[string]string{"event_id": "$name"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/state/m.room.name/") && r.Method == "GET" {
			replyJSON(w, map[string]string{"name": roomName.Load().(string)})
			return
		}
		replyJSON(w, map[string]string{})
	})
	engine := &Engine{c: c, s: s, mx: mx, docker: d, agents: map[string]*agentRuntime{"iris": {busy: map[string]bool{}}}}
	t.Cleanup(engine.wg.Wait)
	return engine, s, names, lock, hermesServer.URL, roomName
}

// The visible name of an operator-created room carries the session's own name, so several
// sessions with the same agent stay distinguishable. The provisioned room keeps the plain
// agent name.
func TestRoomNameCarriesTheSessionName(t *testing.T) {
	engine, s, names, lock, api, _ := namedEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		case "/api/sessions/matrix_room":
			replyJSON(w, map[string]any{"session": map[string]any{"title": "Explain why F9 could not be tested"}})
		default:
			replyJSON(w, map[string]string{"status": "completed", "output": "done"})
		}
	})
	// The agent's own room, and one room the operator created.
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Room = "!home:chat.test"
		m.Rooms = map[string]*Room{
			"!home:chat.test": {},
			"!dm:chat.test":   {},
		}
		m.Sessions = map[string]map[string]*Session{
			"!home:chat.test": {"room": {ID: "matrix_home"}},
			"!dm:chat.test":   {"room": {ID: "matrix_room"}},
		}
		return nil
	})
	engine.refreshRoomName(context.Background(), "iris", "!dm:chat.test", api)
	engine.refreshRoomName(context.Background(), "iris", "!home:chat.test", api)
	lock.Lock()
	written := append([]string{}, *names...)
	lock.Unlock()
	if len(written) != 1 || written[0] != "iris — Explain why F9 could not be tested" {
		t.Fatalf("room name did not carry the session name: %v", written)
	}
	m := s.View().Members["iris"]
	if getSession(m, "!dm:chat.test", "").SessionName != "Explain why F9 could not be tested" {
		t.Fatal("session name was not adopted")
	}
}

// A session name is model-generated text, so it must never reach a room name unsanitised.
func TestSessionNameIsSanitised(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Explain why F9 could not be tested", "Explain why F9 could not be tested"},
		{"line one\nline two", "line one line two"},
		{"  padded  ", "padded"},
		{"trailing punctuation...", "trailing punctuation"},
		{strings.Repeat("é", 100), strings.Repeat("é", 30)},
	}
	for _, tc := range cases {
		if got := cleanSessionTitle(tc.in); got != tc.want {
			t.Fatalf("cleanSessionTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := cleanSessionTitle("bad\x00name\x07here"); strings.ContainsRune(got, 0) || strings.ContainsRune(got, 7) {
		t.Fatalf("control characters survived: %q", got)
	}
}

// A rename moves the agent's name in the shared rooms' names too.
func TestRenameMovesTheSharedRoomNames(t *testing.T) {
	engine, s, names, lock, api, _ := namedEngine(t, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, map[string]any{"session": map[string]any{"title": "Pool exhaustion"}})
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Room = "!home:chat.test"
		m.Rooms = map[string]*Room{"!home:chat.test": {}, "!dm:chat.test": {}}
		m.Sessions = map[string]map[string]*Session{"!dm:chat.test": {"room": {ID: "matrix_room"}}}
		return nil
	})
	engine.refreshRoomName(context.Background(), "iris", "!dm:chat.test", api)
	lock.Lock()
	before := len(*names)
	lock.Unlock()
	if before != 1 {
		t.Fatalf("shared room was not named first: %d", before)
	}
	engine.renameSharedRooms(context.Background(), "iris", "nova")
	lock.Lock()
	written := append([]string{}, *names...)
	lock.Unlock()
	if len(written) != 2 || written[1] != "nova — Pool exhaustion" {
		t.Fatalf("shared room name did not follow the rename: %v", written)
	}
}

// A room name the operator chose is never overwritten by the bridge.
func TestOperatorChosenRoomNameIsKept(t *testing.T) {
	engine, s, names, lock, api, roomName := namedEngine(t, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, map[string]any{"session": map[string]any{"title": "Bridge title"}})
	})
	// The room already carries a name the bridge did not write.
	roomName.Store("Chosen by the operator")
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Room = "!home:chat.test"
		m.Rooms = map[string]*Room{"!home:chat.test": {}, "!dm:chat.test": {}}
		m.Sessions = map[string]map[string]*Session{"!dm:chat.test": {"room": {ID: "matrix_room"}}}
		return nil
	})
	engine.refreshRoomName(context.Background(), "iris", "!dm:chat.test", api)
	lock.Lock()
	written := append([]string{}, *names...)
	lock.Unlock()
	if len(written) != 0 {
		t.Fatalf("the operator's room name was overwritten: %v", written)
	}
}

// The lookup stops on its own: a name that arrived closes the window, and repeated
// lookups do not reopen it.
func TestSessionNameLookupWindowCloses(t *testing.T) {
	engine, s, _, _, api, _ := namedEngine(t, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, map[string]any{"session": map[string]any{"title": "Found"}})
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Room = "!home:chat.test"
		m.Rooms = map[string]*Room{"!home:chat.test": {}, "!dm:chat.test": {}}
		m.Sessions = map[string]map[string]*Session{"!dm:chat.test": {"room": {ID: "matrix_room"}}}
		return nil
	})
	// A turn opens the window.
	s.Update(func(st *State) error {
		getSession(st.Members["iris"], "!dm:chat.test", "").TitleUntil = time.Now().UTC().Add(titleCheckWindow)
		return nil
	})
	engine.refreshRoomName(context.Background(), "iris", "!dm:chat.test", api)
	m := s.View().Members["iris"]
	if time.Now().After(getSession(m, "!dm:chat.test", "").TitleUntil) {
		t.Fatal("the window did not close once a name arrived")
	}
	// A later lookup must not reopen it.
	engine.refreshRoomName(context.Background(), "iris", "!dm:chat.test", api)
	m = s.View().Members["iris"]
	if time.Now().After(getSession(m, "!dm:chat.test", "").TitleUntil) {
		t.Fatal("a later lookup reopened the window")
	}
}

// A completed run names the room the conversation is in.
func TestCompletedRunNamesTheRoom(t *testing.T) {
	engine, s, names, lock, api, _ := namedEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		case "/api/sessions/matrix_room":
			replyJSON(w, map[string]any{"session": map[string]any{"title": "F9 could not be tested"}})
		case "/v1/runs":
			replyJSON(w, map[string]string{"run_id": "run-1", "status": "running", "session_id": "matrix_room"})
		default:
			replyJSON(w, map[string]string{"run_id": "run-1", "status": "completed", "output": "done", "session_id": "matrix_room"})
		}
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Room = "!home:chat.test"
		m.Rooms = map[string]*Room{"!home:chat.test": {}, "!dm:chat.test": {}}
		m.Sessions = map[string]map[string]*Session{"!dm:chat.test": {"room": {ID: "matrix_room"}}}
		m.Turns = []*Turn{{ID: "t1", Room: "!dm:chat.test", Text: "why could F9 not be tested?", Phase: "queued"}}
		return nil
	})
	engine.work(context.Background(), "iris", "t1")
	await(t, func() bool {
		lock.Lock()
		defer lock.Unlock()
		return len(*names) == 1
	})
	lock.Lock()
	written := append([]string{}, *names...)
	lock.Unlock()
	if written[0] != "iris — F9 could not be tested" {
		t.Fatalf("completed run did not name the room: %v", written)
	}
	if findTurn(s.View().Members["iris"], "t1").Phase != "done" {
		t.Fatal("turn did not complete")
	}
	_ = api
}

// The provisioned room keeps the plain agent name: only !fleet rename owns it.
func TestDedicatedRoomKeepsTheAgentName(t *testing.T) {
	if got := roomNameFor("iris", "Some session", true); got != "iris" {
		t.Fatalf("dedicated room name = %q", got)
	}
	if got := roomNameFor("iris", "Some session", false); got != "iris — Some session" {
		t.Fatalf("shared room name = %q", got)
	}
	if got := roomNameFor("iris", "", false); got != "iris" {
		t.Fatalf("unnamed session room name = %q", got)
	}
}

// The name is written once. A second lookup with the same session name must not write again.
func TestRoomNameIsWrittenOnlyWhenItChanges(t *testing.T) {
	engine, s, names, lock, api, _ := namedEngine(t, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, map[string]any{"session": map[string]any{"title": "Stable title"}})
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Room = "!home:chat.test"
		m.Rooms = map[string]*Room{"!home:chat.test": {}, "!dm:chat.test": {}}
		m.Sessions = map[string]map[string]*Session{"!dm:chat.test": {"room": {ID: "matrix_room"}}}
		return nil
	})
	for i := 0; i < 3; i++ {
		engine.refreshRoomName(context.Background(), "iris", "!dm:chat.test", api)
	}
	lock.Lock()
	written := append([]string{}, *names...)
	lock.Unlock()
	if len(written) != 1 {
		t.Fatalf("room name rewritten %d times: %v", len(written), written)
	}
}

// A name that is not there yet leaves the room alone; a later lookup picks it up.
func TestRoomNameWaitsForTheSessionName(t *testing.T) {
	var named atomic.Bool
	engine, s, names, lock, api, _ := namedEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if named.Load() {
			replyJSON(w, map[string]any{"session": map[string]any{"title": "Late title"}})
			return
		}
		replyJSON(w, map[string]any{"session": map[string]any{"title": ""}})
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Room = "!home:chat.test"
		m.Rooms = map[string]*Room{"!home:chat.test": {}, "!dm:chat.test": {}}
		m.Sessions = map[string]map[string]*Session{"!dm:chat.test": {"room": {ID: "matrix_room"}}}
		return nil
	})
	engine.refreshRoomName(context.Background(), "iris", "!dm:chat.test", api)
	lock.Lock()
	early := len(*names)
	lock.Unlock()
	if early != 0 {
		t.Fatal("room was named before the session had a name")
	}
	named.Store(true)
	engine.refreshRoomName(context.Background(), "iris", "!dm:chat.test", api)
	lock.Lock()
	written := append([]string{}, *names...)
	lock.Unlock()
	if len(written) != 1 || written[0] != "iris — Late title" {
		t.Fatalf("late session name was not applied: %v", written)
	}
}

// A room the bot cannot rename must not fail the turn or retry forever.
func TestUnrenamableRoomIsNotFatal(t *testing.T) {
	engine, s, _, _, api, _ := namedEngine(t, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, map[string]any{"session": map[string]any{"title": "Cannot rename"}})
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Room = "!home:chat.test"
		m.Rooms = map[string]*Room{"!home:chat.test": {}, "!dm:chat.test": {}}
		m.Sessions = map[string]map[string]*Session{"!dm:chat.test": {"room": {ID: "matrix_room"}}}
		return nil
	})
	// A homeserver that refuses the write leaves the visible name alone and costs
	// nothing else: the name is display text, not state the bridge depends on.
	engine.refreshRoomName(context.Background(), "iris", "!dm:chat.test", api)
	if s.View().Members["iris"].Rooms["!dm:chat.test"].RoomName == "" {
		t.Fatal("the intended name was not recorded")
	}
}

// The lookup stops once the session's name is settled, so an idle agent is not polled.
func TestSessionNameLookupStopsAfterTheWindow(t *testing.T) {
	engine, s, _, _, api, _ := namedEngine(t, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, map[string]any{"session": map[string]any{"title": "Settled"}})
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Room = "!home:chat.test"
		m.Rooms = map[string]*Room{"!home:chat.test": {}, "!dm:chat.test": {}}
		m.Sessions = map[string]map[string]*Session{"!dm:chat.test": {"room": {ID: "matrix_room"}}}
		return nil
	})
	engine.refreshRoomName(context.Background(), "iris", "!dm:chat.test", api)
	m := s.View().Members["iris"]
	if getSession(m, "!dm:chat.test", "").SessionName == "" {
		t.Fatal("session name was not adopted")
	}
	// A named session keeps a short window open for the model upgrade, then stops.
	s.Update(func(st *State) error {
		s := getSession(st.Members["iris"], "!dm:chat.test", "")
		s.TitleUntil = time.Now().UTC().Add(-time.Second)
		return nil
	})
	if titleDue(s.View().Members["iris"], "!dm:chat.test") {
		t.Fatal("lookup continued after the name settled")
	}
	// A session that was never looked up is looked up once, to learn a name it has.
	s.Update(func(st *State) error {
		s := getSession(st.Members["iris"], "!dm:chat.test", "")
		s.SessionName, s.TitleUntil = "", time.Time{}
		return nil
	})
	if !titleDue(s.View().Members["iris"], "!dm:chat.test") {
		t.Fatal("an unlooked-up session was never checked")
	}
}
