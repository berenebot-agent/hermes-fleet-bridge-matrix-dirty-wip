package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{PublicURL: "https://chat.test", ServerName: "chat.test", Admin: "admin", Password: "private-password-for-tests", Data: t.TempDir(), MatrixListen: "127.0.0.1:18008", Listen: "127.0.0.1:8088", Parallel: 4, Idle: time.Minute, Start: time.Second}
}
func testStore(t *testing.T) (Config, *Store) {
	t.Helper()
	c := testConfig(t)
	s, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if err = s.Update(func(st *State) error {
		st.Members["iris"] = &Member{ID: "iris", Dir: "/members/iris", Project: "iris", Service: "hermes", Enabled: true, Room: "!iris:chat.test", Since: "old-sync", Rooms: map[string]*Room{"!iris:chat.test": {LastEvent: "$old"}}, Sessions: map[string]map[string]*Session{}, LastActivity: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return c, s
}
func fakeMatrix(t *testing.T, c Config, handler http.HandlerFunc) *Matrix {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Matrix{Base: server.URL, c: c, secrets: Secrets{Registration: randomSecret(), Accounts: map[string]*Credential{"admin": {User: "@admin:chat.test", Token: "private-admin-token"}, "agent_iris": {User: "@agent_iris:chat.test", Token: "private-bot-token"}}, APIKeys: map[string]string{"iris": randomSecret()}}}
}
func replyJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonBytes(v))
}
func message(id, sender, text, thread string) map[string]any {
	content := map[string]any{"msgtype": "m.text", "body": text}
	if thread != "" {
		content["m.relates_to"] = map[string]string{"rel_type": "m.thread", "event_id": thread}
	}
	return map[string]any{"event_id": id, "sender": sender, "type": "m.room.message", "content": content}
}
func syncResponse(events []any, limited bool) map[string]any {
	return map[string]any{"next_batch": "next-sync", "rooms": map[string]any{"join": map[string]any{"!iris:chat.test": map[string]any{"timeline": map[string]any{"events": events, "limited": limited, "prev_batch": "gap-start"}}}}}
}

// syncRooms builds a sync response covering several joined rooms at once.
func syncRooms(rooms map[string]any) map[string]any {
	return map[string]any{"next_batch": "next-sync", "rooms": map[string]any{"join": rooms}}
}

// inviteEvent is a membership invite for the agent from the given sender.
func inviteEvent(sender string) map[string]any {
	return map[string]any{"type": "m.room.member", "sender": sender, "state_key": "@agent_iris:chat.test", "content": map[string]any{"membership": "invite"}}
}

// syncInvites builds a sync response with pending invites only.
func syncInvites(rooms ...string) map[string]any {
	invites := map[string]any{}
	for _, room := range rooms {
		invites[room] = map[string]any{"invite_state": map[string]any{"events": []any{inviteEvent("@admin:chat.test")}}}
	}
	return map[string]any{"next_batch": "next-sync", "rooms": map[string]any{"invite": invites}}
}

// addressed is a member message carrying an explicit Matrix mention of the agent.
func addressed(id, text string) map[string]any {
	m := message(id, "@admin:chat.test", text, "")
	m["content"].(map[string]any)["m.mentions"] = map[string]any{"user_ids": []string{"@agent_iris:chat.test"}}
	return m
}
func TestConfiguration(t *testing.T) {
	t.Setenv("PUBLIC_URL", "https://chat.test")
	t.Setenv("MATRIX_SERVER_NAME", "chat.test")
	t.Setenv("ADMIN_PASSWORD", "private-password-for-tests")
	t.Setenv("HERMES_DIRS", "")
	t.Setenv("HERMES_OVERRIDES", "")
	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"http://chat.test", "https://user:secret@chat.test", "https://chat.test/a", "https://chat.test?key=private"} {
		t.Setenv("PUBLIC_URL", bad)
		if _, err := loadConfig(); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid URL accepted or leaked")
		}
	}
	t.Setenv("PUBLIC_URL", "https://chat.test")
	t.Setenv("MATRIX_SERVER_NAME", "other.test")
	if c, err := loadConfig(); err != nil || c.ServerName != "other.test" || c.PublicURL != "https://chat.test" {
		t.Fatal("separate public origin and Matrix identity rejected")
	}
	t.Setenv("MATRIX_SERVER_NAME", "chat.test")
	// A mixed-case ADMIN_USERNAME is normalised, not refused: the homeserver lowercases
	// localparts, so the stored account must match the one login is checked against.
	t.Setenv("ADMIN_USERNAME", "  Ben ")
	c, err := loadConfig()
	if err != nil || c.Admin != "ben" {
		t.Fatalf("mixed-case ADMIN_USERNAME not normalised: admin=%q err=%v", c.Admin, err)
	}
	// The reserved namespace is refused regardless of the casing used to write it.
	for _, bad := range []string{"agent_iris", "AGENT_iris", "Agent_iris", "not a localpart", "ben!"} {
		t.Setenv("ADMIN_USERNAME", bad)
		if _, err := loadConfig(); err == nil {
			t.Fatalf("reserved or malformed ADMIN_USERNAME accepted: %q", bad)
		}
	}
}
func TestEnvPreservesCredentialsAndUnrelatedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	key := randomSecret()
	original := "# operator comment\nOTHER='keep\nAPI_SERVER_KEY=inside-quoted-value\nverbatim'\nAPI_SERVER_KEY=\"" + key + "\"\nX=a=b\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := configureEnv(path, "0.0.0.0", 8642)
		if err != nil || got != key {
			t.Fatal("credential changed", err)
		}
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "OTHER='keep\nAPI_SERVER_KEY=inside-quoted-value\nverbatim'") || !strings.Contains(string(b), "X=a=b") || strings.Count(string(b), "API_SERVER_ENABLED=") != 1 {
		t.Fatal("unrelated configuration damaged")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("private permissions missing")
	}
}
func TestEnvFailsClosed(t *testing.T) {
	for _, body := range []string{"API_SERVER_KEY=weak\n", "API_SERVER_ENABLED=true\nAPI_SERVER_ENABLED=false\n", "OTHER='unterminated\n", "API_SERVER_KEY='multiline\nvalue'\n"} {
		path := filepath.Join(t.TempDir(), ".env")
		os.WriteFile(path, []byte(body), 0600)
		if _, err := configureEnv(path, "127.0.0.1", 8642); err == nil {
			t.Fatal("unsafe dotenv accepted")
		}
		b, _ := os.ReadFile(path)
		if string(b) != body {
			t.Fatal("failed validation changed dotenv")
		}
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("KEEP=yes\n"), 0600)
	os.Symlink(target, filepath.Join(dir, ".env"))
	if _, err := configureEnv(filepath.Join(dir, ".env"), "127.0.0.1", 8642); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestEscapedMultilineQuotesDoNotExposeManagedLookingLines(t *testing.T) {
	for _, quote := range []string{"'", `"`} {
		t.Run(quote, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			key := randomSecret()
			original := "OTHER=" + quote + "start\nescaped \\" + quote + " quote\nAPI_SERVER_HOST=inside-other-value\nlast" + quote + "\nAPI_SERVER_KEY=" + key + "\n"
			os.WriteFile(path, []byte(original), 0600)
			got, err := configureEnv(path, "0.0.0.0", 8642)
			if err != nil || got != key {
				t.Fatal("escaped multiline dotenv was not preserved", err)
			}
			updated, _ := os.ReadFile(path)
			if !strings.HasPrefix(string(updated), original) {
				t.Fatal("managed-looking text inside a quoted value was rewritten")
			}
		})
	}
}
func TestStateLockIdentityAndCorruption(t *testing.T) {
	c := testConfig(t)
	s, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = openStore(c); err == nil {
		t.Fatal("second writer accepted")
	}
	s.Close()
	c.ServerName = "other.test"
	if _, err = openStore(c); err == nil {
		t.Fatal("renamed identity accepted")
	}
	c.ServerName = "chat.test"
	os.WriteFile(filepath.Join(c.Data, "controller", "state.json"), []byte("{"), 0600)
	if _, err = openStore(c); err == nil {
		t.Fatal("corruption reset state")
	}
}
func TestMissingStateCannotReplayExistingMatrix(t *testing.T) {
	c := testConfig(t)
	os.MkdirAll(filepath.Join(c.Data, "matrix"), 0700)
	os.WriteFile(filepath.Join(c.Data, "matrix", "existing-db"), []byte("x"), 0600)
	if _, err := openStore(c); err == nil {
		t.Fatal("existing database silently reinitialized")
	}
}
func TestStateWriteFailurePoisonsWriter(t *testing.T) {
	_, s := testStore(t)
	before, _ := os.ReadFile(s.path)
	s.path = filepath.Join(t.TempDir(), "missing", "state.json")
	if err := s.Update(func(st *State) error { st.Members["iris"].Since = "unsafe"; return nil }); err == nil {
		t.Fatal("durability failure ignored")
	}
	if err := s.Update(func(*State) error { return nil }); err == nil {
		t.Fatal("poisoned writer continued")
	}
	select {
	case <-s.Fatal:
	default:
		t.Fatal("fatal not signalled")
	}
	var prior State
	if json.Unmarshal(before, &prior) != nil || prior.Members["iris"].Since != "old-sync" {
		t.Fatal("previous committed cursor changed")
	}
}

// Membership is the access boundary: any human in a room may talk to the agent, while bot
// accounts never wake it. Duplicate events and non-text relations stay ignored.
func TestSyncAcceptsRoomMembersAndIgnoresBots(t *testing.T) {
	c, s := testStore(t)
	admin := "@admin:chat.test"
	events := []any{message("$one", admin, "hello", ""), message("$member", "@teammate:chat.test", "wake", ""), message("$bot", "@agent_iris:chat.test", "loop", ""), message("$otherbot", "@agent_other:chat.test", "loop", ""), message("$thread", admin, "explain", "$one"), map[string]any{"event_id": "$edit", "sender": admin, "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": "* edit", "m.relates_to": map[string]string{"rel_type": "m.replace", "event_id": "$one"}}}}
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/event/") {
			replyJSON(w, message("$one", admin, "root text", ""))
			return
		}
		replyJSON(w, syncResponse(events, false))
	})
	for i := 0; i < 2; i++ {
		if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
			t.Fatal(err)
		}
	}
	m := s.View().Members["iris"]
	if len(m.Turns) != 3 || m.Since != "next-sync" {
		t.Fatalf("bad dispatch/dedup count %d", len(m.Turns))
	}
	if m.Turns[2].Phase != "done" || m.Turns[2].Thread != "$one" || m.Turns[1].Room != "!iris:chat.test" || len(m.Outbox) != 1 {
		t.Fatal("thread was not explicitly rejected or room context lost")
	}
	a := getSession(m, m.Room, "").ID
	b := getSession(m, m.Room, "$one").ID
	if a == b || getSession(m, m.Room, "$one").ID != b {
		t.Fatal("session mapping unstable")
	}
}
func TestSyncBackfillsLimitedTimelineInOrder(t *testing.T) {
	c, s := testStore(t)
	admin := "@admin:chat.test"
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			replyJSON(w, map[string]any{"chunk": []any{message("$middle", admin, "second", ""), message("$first", admin, "first", ""), message("$old", admin, "already seen", "")}, "end": "end"})
			return
		}
		replyJSON(w, syncResponse([]any{message("$last", admin, "third", "")}, true))
	})
	if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	turns := s.View().Members["iris"].Turns
	if len(turns) != 3 || turns[0].ID != "$first" || turns[1].ID != "$middle" || turns[2].ID != "$last" {
		t.Fatal("limited sync dropped or reordered messages")
	}
}
func TestUnbridgeableGapRetainsCursor(t *testing.T) {
	c, s := testStore(t)
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			replyJSON(w, map[string]any{"chunk": []any{}, "end": "end"})
			return
		}
		replyJSON(w, syncResponse(nil, true))
	})
	if mx.syncOnce(context.Background(), s, "iris") == nil {
		t.Fatal("history gap ignored")
	}
	if s.View().Members["iris"].Since != "old-sync" {
		t.Fatal("cursor advanced across missing history")
	}
}
func TestFullQueueStillAcceptsStop(t *testing.T) {
	c, s := testStore(t)
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		for i := 0; i < maxPending; i++ {
			m.Turns = append(m.Turns, &Turn{ID: strconv.Itoa(i), Phase: "queued"})
		}
		return nil
	})
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, syncResponse([]any{message("$overflow", "@admin:chat.test", "wake", ""), message("$stop", "@admin:chat.test", "!stop", "")}, false))
	})
	if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	m := s.View().Members["iris"]
	if findTurn(m, "$overflow").Phase != "done" || findTurn(m, "$stop").Phase != "queued" {
		t.Fatal("queue bound blocked safety control")
	}
}
func TestReplyTransactionSurvivesLostAck(t *testing.T) {
	c, s := testStore(t)
	var paths []string
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if len(paths) == 1 {
			w.WriteHeader(500)
			return
		}
		replyJSON(w, map[string]string{"event_id": "$reply"})
	})
	s.Update(func(st *State) error {
		addNotice(st.Members["iris"], "stable-turn", "!iris:chat.test", "$root", "answer")
		return nil
	})
	mx.sendNext(context.Background(), s, "iris")
	s.Update(func(st *State) error { st.Members["iris"].Outbox[0].Next = time.Time{}; return nil })
	mx.sendNext(context.Background(), s, "iris")
	if len(paths) != 2 || paths[0] != paths[1] || len(s.View().Members["iris"].Outbox) != 0 {
		t.Fatal("reply transaction not stable")
	}
}
func TestUnicodeChunking(t *testing.T) {
	text := strings.Repeat("😀", 9000)
	parts := chunks(text)
	if strings.Join(parts, "") != text {
		t.Fatal("chunking changed text")
	}
	for _, p := range parts {
		if len(p) > 12000 || !utf8.ValidString(p) {
			t.Fatal("invalid chunk")
		}
	}
}
func TestFrontendDoesNotExposeAdminOrSecrets(t *testing.T) {
	c, s := testStore(t)
	c.Assets = t.TempDir()
	os.WriteFile(filepath.Join(c.Assets, "index.html"), []byte("Element fixture"), 0600)
	var ready atomic.Bool
	ready.Store(true)
	h := frontend(c, s, &ready)
	for _, path := range []string{"/_synapse/admin/v1/register", "/_matrix/federation/v1/version", "/_matrix/client/v3/admin/whois/foo", "/.well-known/matrix/server"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("admin/federation path exposed: %s", path)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/config.json", nil))
	if strings.Contains(w.Body.String(), c.Password) || !strings.Contains(w.Body.String(), `"disable_custom_urls":true`) {
		t.Fatal("unsafe web config")
	}
	ready.Store(false)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 {
		t.Fatal("UI available before rooms ready")
	}
}

type rewriteTransport struct {
	base      *url.URL
	transport http.RoundTripper
}

func (r rewriteTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	copy := q.Clone(q.Context())
	u := *q.URL
	copy.URL = &u
	copy.URL.Scheme = r.base.Scheme
	copy.URL.Host = r.base.Host
	return r.transport.RoundTrip(copy)
}
func fakeEngine(t *testing.T, handler http.HandlerFunc) (*Engine, *Store) {
	t.Helper()
	c, s := testStore(t)
	hermes := httptest.NewServer(handler)
	t.Cleanup(hermes.Close)
	u, _ := url.Parse(hermes.URL)
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
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) { replyJSON(w, map[string]string{}) })
	engine := &Engine{c: c, s: s, mx: mx, docker: d, agents: map[string]*agentRuntime{"iris": {busy: map[string]bool{}}}}
	t.Cleanup(engine.wg.Wait)
	return engine, s
}
func fakeCapabilities(w http.ResponseWriter) {
	replyJSON(w, map[string]any{"features": map[string]any{"runs_idempotency": map[string]any{"supported": true, "durable": true, "retention_seconds": 86400}}})
}
func fakeHealth(w http.ResponseWriter) {
	replyJSON(w, map[string]any{"status": "ok", "gateway_busy": false, "gateway_drainable": true, "active_agents": 0, "readiness": map[string]any{"status": "ok", "checks": map[string]any{"background_queues": map[string]int{"active_api_runs": 0, "process_completions": 0, "active_delegations": 0}}}})
}
func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
func TestParallelSessionsButFIFOWithinSession(t *testing.T) {
	var mu sync.Mutex
	inputs := map[string]string{}
	released := false
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		case "/v1/runs":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			inputs[body["input"]] = body["session_id"]
			mu.Unlock()
			replyJSON(w, map[string]string{"run_id": stableID(body["input"]), "status": "running"})
		default:
			mu.Lock()
			done := released
			mu.Unlock()
			status := "running"
			if done {
				status = "completed"
			}
			replyJSON(w, map[string]string{"status": status, "output": "done"})
		}
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Turns = []*Turn{{ID: "a1", Thread: "$a", Text: "A1", Root: "root A", Phase: "queued"}, {ID: "a2", Thread: "$a", Text: "A2", Phase: "queued"}, {ID: "b1", Thread: "$b", Text: "B1", Root: "root B", Phase: "queued"}}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.tick(ctx, "iris")
	await(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(inputs) == 2 })
	engine.tick(ctx, "iris")
	mu.Lock()
	if len(inputs) != 2 {
		t.Error("same-session turn ran concurrently")
	}
	released = true
	mu.Unlock()
	await(t, func() bool {
		m := s.View().Members["iris"]
		return findTurn(m, "a1").Phase == "done" && findTurn(m, "b1").Phase == "done"
	})
	await(t, func() bool { rt := engine.agents["iris"]; rt.mu.Lock(); defer rt.mu.Unlock(); return len(rt.busy) == 0 })
	engine.tick(ctx, "iris")
	await(t, func() bool { return findTurn(s.View().Members["iris"], "a2").Phase == "done" })
	mu.Lock()
	defer mu.Unlock()
	if len(inputs) != 3 {
		t.Fatal("second FIFO turn missing")
	}
	for text, session := range inputs {
		if strings.Contains(text, "A1") && (!strings.Contains(text, "root A") || session == "") {
			t.Fatal("thread root/session missing")
		}
	}
}
func TestRestartKnownRunDoesNotResubmit(t *testing.T) {
	var posts atomic.Int32
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		case "/v1/runs":
			posts.Add(1)
			replyJSON(w, map[string]string{"run_id": "wrong"})
		default:
			replyJSON(w, map[string]string{"status": "completed", "output": "recovered answer"})
		}
	})
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "restart", Phase: "running", RunID: "existing-run"}}
		return nil
	})
	engine.work(context.Background(), "iris", "restart")
	m := s.View().Members["iris"]
	if posts.Load() != 0 || findTurn(m, "restart").Phase != "done" || m.Outbox[0].Text != "recovered answer" {
		t.Fatal("restart resubmitted or lost reply")
	}
}
func TestExpiredSubmissionHeldWithoutReplay(t *testing.T) {
	var posts atomic.Int32
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		default:
			posts.Add(1)
			replyJSON(w, map[string]string{"run_id": "unsafe"})
		}
	})
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "uncertain", Phase: "submitting", Submitted: time.Now().Add(-48 * time.Hour), Retention: 86400, Request: json.RawMessage(`{"input":"do not repeat"}`)}}
		return nil
	})
	engine.work(context.Background(), "iris", "uncertain")
	if posts.Load() != 0 || findTurn(s.View().Members["iris"], "uncertain").Phase != "held" {
		t.Fatal("expired request replayed")
	}
}
func TestStopOnlyCancelsOriginatingSession(t *testing.T) {
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) { replyJSON(w, map[string]string{"status": "stopping"}) })
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "one", Thread: "$a", Phase: "running", RunID: "r1"}, {ID: "two", Thread: "$a", Phase: "queued"}, {ID: "other", Thread: "$b", Phase: "running", RunID: "r2"}, {ID: "control", Thread: "$a", Text: "!stop", Kind: "command", Phase: "queued"}}
		return nil
	})
	engine.command(context.Background(), "iris", "control")
	m := s.View().Members["iris"]
	if !findTurn(m, "one").Stop || findTurn(m, "two").Phase != "done" || findTurn(m, "other").Stop {
		t.Fatal("stop crossed session boundary")
	}
}
func TestNoSecretInRemoteErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, `{"errcode":"private-password","error":"private-token"}`)
	}))
	defer server.Close()
	err := api(context.Background(), "GET", server.URL, "/", "", nil, nil, nil)
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("remote error leaked content")
	}
}

func TestLostAdmissionAckReplaysIdenticalRequest(t *testing.T) {
	var mu sync.Mutex
	var keys, bodies []string
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		case "/v1/runs":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			bodies = append(bodies, string(b))
			count := len(keys)
			mu.Unlock()
			if count == 1 {
				w.WriteHeader(500)
				return
			}
			replyJSON(w, map[string]string{"run_id": "original-run"})
		default:
			replyJSON(w, map[string]string{"status": "completed", "output": "once"})
		}
	})
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "lost-ack", Phase: "submitting", Submitted: time.Now(), Retention: 86400, Request: json.RawMessage(`{"input":"immutable","session_id":"stable"}`)}}
		return nil
	})
	engine.work(context.Background(), "iris", "lost-ack")
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] || bodies[0] != bodies[1] {
		t.Fatal("lost admission changed logical request")
	}
	if findTurn(s.View().Members["iris"], "lost-ack").RunID != "original-run" {
		t.Fatal("original run not recovered")
	}
}
func TestReducedRemoteRetentionPreventsReplay(t *testing.T) {
	var posts atomic.Int32
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			replyJSON(w, map[string]any{"features": map[string]any{"runs_idempotency": map[string]any{"supported": true, "durable": true, "retention_seconds": 300}}})
		case "/health/detailed":
			fakeHealth(w)
		default:
			posts.Add(1)
			replyJSON(w, map[string]string{"run_id": "unsafe"})
		}
	})
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "old", Phase: "submitting", Submitted: time.Now().Add(-10 * time.Minute), Retention: 86400, Request: json.RawMessage(`{"input":"not again"}`)}}
		return nil
	})
	engine.work(context.Background(), "iris", "old")
	if posts.Load() != 0 || findTurn(s.View().Members["iris"], "old").Phase != "held" {
		t.Fatal("remote retention downgrade allowed duplicate execution")
	}
}
func TestApprovalIsExactOnceAndConversationScoped(t *testing.T) {
	var approvals atomic.Int32
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/approval") {
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			if b["request_id"] != "exact-request" || b["choice"] != "once" {
				t.Error("approval widened")
			}
			approvals.Add(1)
			replyJSON(w, map[string]int{"resolved": 1})
			return
		}
		replyJSON(w, map[string]any{"status": "waiting_for_approval", "approval": map[string]string{"request_id": "exact-request"}})
	})
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "run", Thread: "$a", Phase: "running", RunID: "r1", Approval: "exact-request"}, {ID: "wrong-room", Thread: "$b", Kind: "command", Phase: "queued", Text: "!approve exact-request"}, {ID: "correct", Thread: "$a", Kind: "command", Phase: "queued", Text: "!approve exact-request"}}
		return nil
	})
	engine.command(context.Background(), "iris", "wrong-room")
	if approvals.Load() != 0 {
		t.Fatal("approval crossed thread boundary")
	}
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		findTurn(m, "run").Stop = true
		m.Turns = append(m.Turns, &Turn{ID: "after-stop", Thread: "$a", Kind: "command", Phase: "queued", Text: "!approve exact-request"})
		return nil
	})
	engine.command(context.Background(), "iris", "after-stop")
	if approvals.Load() != 0 {
		t.Fatal("cancelled run accepted a new approval")
	}
	s.Update(func(st *State) error { findTurn(st.Members["iris"], "run").Stop = false; return nil })
	engine.command(context.Background(), "iris", "correct")
	if approvals.Load() != 1 {
		t.Fatal("exact approval missing")
	}
}
func TestDiscardExpiredRunRequiresAuthenticatedGlobalInactivity(t *testing.T) {
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		default:
			w.WriteHeader(404)
		}
	})
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "held", Phase: "held", RunID: "expired-record"}, {ID: "discard", Kind: "command", Phase: "queued", Text: "!discard held"}}
		return nil
	})
	engine.command(context.Background(), "iris", "discard")
	if findTurn(s.View().Members["iris"], "held").Phase != "done" {
		t.Fatal("terminal record expiry made recovery impossible")
	}
}
func TestModelCatalogAndSessionProvider(t *testing.T) {
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		default:
			replyJSON(w, map[string]any{"providers": []any{map[string]any{"slug": "custom:local", "models": []string{"model-a", "model-b"}}}})
		}
	})
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "models", Sender: menuOwner, Kind: "command", Phase: "queued", Text: "!models"}}
		return nil
	})
	engine.command(context.Background(), "iris", "models")
	menuCommandTest(t, engine, "choose", "!iris:chat.test", "!model model-b custom:local")
	menuPickTest(t, engine, "scope", "choose-menu", "1️⃣")
	m := s.View().Members["iris"]
	if !strings.Contains(m.Outbox[0].Text, "model-b") || getSession(m, m.Room, "").Provider != "custom:local" || getSession(m, m.Room, "$a").Model != "" {
		t.Fatal("model catalog or session scoping broken")
	}
}
func TestIdleShutdownRequiresAllActivityCounters(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if busy.Load() {
			replyJSON(w, map[string]any{"gateway_busy": false, "gateway_drainable": true, "active_agents": 0, "readiness": map[string]any{"checks": map[string]any{"background_queues": map[string]int{"active_api_runs": 0, "process_completions": 1, "active_delegations": 0}}}})
		} else {
			fakeHealth(w)
		}
	})
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0700)
	called := filepath.Join(dir, "called")
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+called+"\n"), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	engine.c.Idle = time.Second
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Dir = dir
		m.Files = []string{filepath.Join(dir, "docker-compose.yml")}
		m.LastActivity = time.Now().Add(-time.Hour)
		return nil
	})
	engine.idle(context.Background(), "iris")
	if _, err := os.Stat(called); err == nil {
		t.Fatal("background completion was killed")
	}
	busy.Store(false)
	engine.idle(context.Background(), "iris")
	b, err := os.ReadFile(called)
	if err != nil || !strings.Contains(string(b), "down") {
		t.Fatal("confirmed idle agent not stopped")
	}
}
func TestGofmtWrapperFailsWhenFilesAreListed(t *testing.T) {
	for _, listed := range []bool{false, true} {
		t.Run(strconv.FormatBool(listed), func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			os.Mkdir(bin, 0700)
			os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho Pulling-test-image >&2\nwhile [ \"$#\" -gt 0 ] && [ \"$1\" != sh ]; do shift; done\nexec \"$@\"\n"), 0700)
			formatter := "#!/bin/sh\nexit 0\n"
			if listed {
				formatter = "#!/bin/sh\necho needs-format.go\n"
			}
			os.WriteFile(filepath.Join(bin, "gofmt"), []byte(formatter), 0700)
			cmd := exec.Command("bash", "./hermes-fleet-bridge-matrix-go.sh", "gofmt", "-l", ".")
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HFB_GO_CACHE="+filepath.Join(dir, "cache"), "HFB_TEST_DIR="+filepath.Join(dir, "logs"))
			if failed := cmd.Run() != nil; failed != listed {
				t.Fatal("format gate confused Docker pull noise with formatter output")
			}
		})
	}
}

func TestAmbiguousRunningServiceNeverTriggersComposeUp(t *testing.T) {
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) { fakeHealth(w) })
	dir := t.TempDir()
	s.Update(func(st *State) error { st.Members["iris"].Dir = dir; return nil })
	labels := map[string]string{"com.docker.compose.project": "iris", "com.docker.compose.service": "hermes", "com.docker.compose.project.working_dir": dir}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/json" {
			replyJSON(w, []any{map[string]any{"Id": "one", "Labels": labels}, map[string]any{"Id": "two", "Labels": labels}})
		} else {
			replyJSON(w, map[string]any{"Config": map[string]any{"Labels": labels}, "HostConfig": map[string]string{"NetworkMode": "host"}})
		}
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	engine.docker.client = &http.Client{Transport: rewriteTransport{u, http.DefaultTransport}}
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0700)
	called := filepath.Join(dir, "called")
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho unsafe > "+called+"\n"), 0700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if _, _, err := engine.ensure(context.Background(), "iris", ""); err == nil {
		t.Fatal("ambiguous replicas accepted")
	}
	if _, err := os.Stat(called); err == nil {
		t.Fatal("ambiguous running service was recreated")
	}
}

func TestProvisionRecoversAliasBeforeLocalMapping(t *testing.T) {
	c, s := testStore(t)
	s.Update(func(st *State) error { st.Members["iris"].Room = ""; return nil })
	var created atomic.Int32
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/whoami"):
			replyJSON(w, map[string]string{"user_id": "@agent_iris:chat.test"})
		case strings.Contains(r.URL.Path, "/directory/room/"):
			replyJSON(w, map[string]string{"room_id": "!recovered:chat.test"})
		case strings.HasSuffix(r.URL.Path, "/org.hermes.fleet/"):
			replyJSON(w, map[string]string{"agent": "iris", "server": "chat.test"})
		case strings.Contains(r.URL.Path, "/state/m.room.member/"):
			replyJSON(w, map[string]string{"membership": "join"})
		case strings.Contains(r.URL.Path, "/state/m.room.power_levels/"):
			if r.Method == "PUT" {
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if users, _ := body["users"].(map[string]any); users["@admin:chat.test"] != float64(100) {
					t.Error("administrator power level was not granted")
				}
			}
			replyJSON(w, map[string]any{"users": map[string]any{}})
		case strings.Contains(r.URL.Path, "/profile/"):
			replyJSON(w, map[string]any{"displayname": "iris"})
		default:
			created.Add(1)
			w.WriteHeader(500)
		}
	})
	if err := mx.provision(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	if created.Load() != 0 || s.View().Members["iris"].Room != "!recovered:chat.test" {
		t.Fatal("provisioning duplicated a previously-created room")
	}
}
func TestSavedRoomDoesNotDependOnAlias(t *testing.T) {
	c, s := testStore(t)
	var created atomic.Int32
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/whoami"):
			replyJSON(w, map[string]string{"user_id": "@agent_iris:chat.test"})
		case strings.HasSuffix(r.URL.Path, "/org.hermes.fleet/"):
			replyJSON(w, map[string]string{"agent": "iris", "server": "chat.test"})
		case strings.Contains(r.URL.Path, "/state/m.room.member/"):
			replyJSON(w, map[string]string{"membership": "join"})
		case strings.Contains(r.URL.Path, "/state/m.room.power_levels/"):
			replyJSON(w, map[string]any{"users": map[string]any{}})
		case strings.Contains(r.URL.Path, "/profile/"):
			replyJSON(w, map[string]any{"displayname": "agent_iris"})
		default:
			created.Add(1)
			w.WriteHeader(404)
		}
	})
	if err := mx.provision(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	if created.Load() != 0 || s.View().Members["iris"].Room != "!iris:chat.test" {
		t.Fatal("missing alias replaced a saved room")
	}
}

// Reconciliation repairs an agent's room power levels and bot display name, and skips both when
// they are already correct, so a restart does not rewrite state it does not need to change.
func TestProvisionRepairsRoomPowerAndBotNameOnce(t *testing.T) {
	cases := []struct {
		name       string
		levels     map[string]any
		display    string
		plWrites   int32
		nameWrites int32
	}{
		{"repairs", map[string]any{}, "agent_iris", 1, 1},
		{"already correct", map[string]any{"@admin:chat.test": float64(100)}, "iris", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, s := testStore(t)
			var puts, names atomic.Int32
			mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/whoami"):
					replyJSON(w, map[string]string{"user_id": "@agent_iris:chat.test"})
				case strings.HasSuffix(r.URL.Path, "/org.hermes.fleet/"):
					replyJSON(w, map[string]string{"agent": "iris", "server": "chat.test"})
				case strings.Contains(r.URL.Path, "/state/m.room.member/"):
					replyJSON(w, map[string]string{"membership": "join"})
				case strings.Contains(r.URL.Path, "/state/m.room.power_levels/"):
					if r.Method == "PUT" {
						puts.Add(1)
					}
					replyJSON(w, map[string]any{"users": tc.levels})
				case strings.Contains(r.URL.Path, "/profile/"):
					if r.Method == "PUT" {
						names.Add(1)
						var body map[string]string
						json.NewDecoder(r.Body).Decode(&body)
						if body["displayname"] != "iris" {
							t.Errorf("display name %q does not match the agent", body["displayname"])
						}
					}
					replyJSON(w, map[string]any{"displayname": tc.display})
				default:
					replyJSON(w, map[string]any{})
				}
			})
			if err := mx.provision(context.Background(), s, "iris"); err != nil {
				t.Fatal(err)
			}
			if puts.Load() != tc.plWrites || names.Load() != tc.nameWrites {
				t.Fatalf("power writes = %d (want %d), name writes = %d (want %d)", puts.Load(), tc.plWrites, names.Load(), tc.nameWrites)
			}
			if s.View().Members["iris"].Rooms["!iris:chat.test"] == nil {
				t.Fatal("provisioned room not tracked")
			}
		})
	}
}

func TestRejectedFirstThreadTurnKeepsRootForExplicitRetry(t *testing.T) {
	var mu sync.Mutex
	var inputs []string
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		case "/v1/runs":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			inputs = append(inputs, body["input"])
			attempt := len(inputs)
			mu.Unlock()
			if attempt == 1 {
				w.WriteHeader(400)
			} else {
				replyJSON(w, map[string]string{"run_id": "accepted-retry"})
			}
		default:
			replyJSON(w, map[string]string{"status": "completed", "output": "done"})
		}
	})
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "first", Thread: "$root", Text: "try", Root: "quoted-root-context", Phase: "queued"}}
		return nil
	})
	engine.work(context.Background(), "iris", "first")
	if getSession(s.View().Members["iris"], "!iris:chat.test", "$root").Seeded {
		t.Fatal("rejected request consumed thread context")
	}
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: "discard", Thread: "$root", Text: "!discard first", Kind: "command", Phase: "queued"})
		return nil
	})
	engine.command(context.Background(), "iris", "discard")
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: "retry", Thread: "$root", Text: "try again", Root: "quoted-root-context", Phase: "queued"})
		return nil
	})
	engine.work(context.Background(), "iris", "retry")
	mu.Lock()
	defer mu.Unlock()
	if len(inputs) != 2 || !strings.Contains(inputs[1], "quoted-root-context") || !getSession(s.View().Members["iris"], "!iris:chat.test", "$root").Seeded {
		t.Fatal("explicit retry lost initial thread context")
	}
}

func TestColdReadinessDoesNotBlockStatusOrStop(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var released sync.Once
	defer released.Do(func() { close(release) })
	var typing atomic.Bool
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			if !typing.Load() {
				t.Error("typing was not started before readiness")
			}
			once.Do(func() {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		default:
			replyJSON(w, map[string]string{"status": "completed", "run_id": "unexpected"})
		}
	})
	engine.mx = fakeMatrix(t, engine.c, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/typing/") {
			typing.Store(true)
		}
		replyJSON(w, map[string]any{})
	})
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "cold", Phase: "queued", Text: "start"}}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.tick(ctx, "iris")
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("readiness probe did not start")
	}
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: "status", Kind: "command", Phase: "queued", Text: "!status"}, &Turn{ID: "stop", Kind: "command", Phase: "queued", Text: "!stop"})
		return nil
	})
	tickDone := make(chan struct{})
	go func() { engine.tick(ctx, "iris"); close(tickDone) }()
	select {
	case <-tickDone:
	case <-time.After(time.Second):
		t.Fatal("lifecycle lock blocked the control plane")
	}
	await(t, func() bool {
		m := s.View().Members["iris"]
		return findTurn(m, "status").Phase == "done" && findTurn(m, "stop").Phase == "done" && findTurn(m, "cold").Phase == "done"
	})
	released.Do(func() { close(release) })
}
func TestStaleDispatchSnapshotCannotWakeCompletedTurn(t *testing.T) {
	var requests atomic.Int32
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); fakeCapabilities(w) })
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "cancelled", Phase: "done"}}
		return nil
	})
	engine.work(context.Background(), "iris", "cancelled")
	engine.fail("iris", "cancelled", "late startup failure", true)
	if requests.Load() != 0 || findTurn(s.View().Members["iris"], "cancelled").Phase != "done" || len(s.View().Members["iris"].Outbox) > 0 {
		t.Fatal("stale dispatch woke or held a completed turn")
	}
}

// Membership is the access boundary: the bot joins rooms an approved human adds it to and
// never follows an invite from another bot.
func TestInvitesAcceptedFromHumansOnly(t *testing.T) {
	c, s := testStore(t)
	var joined []string
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/join/") {
			joined = append(joined, strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/join/"))
			replyJSON(w, map[string]string{"room_id": "joined"})
			return
		}
		body := syncInvites("!team:chat.test", "!hostile:chat.test", "!botted:chat.test")
		invites := body["rooms"].(map[string]any)["invite"].(map[string]any)
		invites["!botted:chat.test"] = map[string]any{"invite_state": map[string]any{"events": []any{inviteEvent("@agent_other:chat.test")}}}
		replyJSON(w, body)
	})
	if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	accepted := map[string]bool{}
	for _, room := range joined {
		accepted[room] = true
	}
	if len(joined) != 2 || !accepted["!team:chat.test"] || !accepted["!hostile:chat.test"] {
		t.Fatalf("invite acceptance wrong: %v", joined)
	}
}

// A mention is required in rooms the operator created, never required in the agent's own room,
// and a space is a container that is joined but never dispatched from.
func TestMentionPolicyAndSpaceContainers(t *testing.T) {
	c, s := testStore(t)
	team, space := "!team:chat.test", "!space:chat.test"
	s.Update(func(st *State) error {
		st.Members["iris"].Rooms[team] = &Room{Mention: true}
		st.Members["iris"].Rooms[space] = &Room{Mention: true, Space: true}
		return nil
	})
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, syncRooms(map[string]any{
			"!iris:chat.test": map[string]any{"timeline": map[string]any{"events": []any{message("$own", "@admin:chat.test", "no mention needed", "")}}},
			team:              map[string]any{"timeline": map[string]any{"events": []any{message("$plain", "@admin:chat.test", "unaddressed", ""), addressed("$mentioned", "@agent_iris:chat.test: summarise this")}}},
			space:             map[string]any{"timeline": map[string]any{"events": []any{addressed("$inspace", "hello space")}}},
		}))
	})
	if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	m := s.View().Members["iris"]
	if findTurn(m, "$plain") != nil || findTurn(m, "$inspace") != nil || findTurn(m, "$bot") != nil {
		t.Fatal("mention policy or space container ignored")
	}
	if got := findTurn(m, "$own"); got == nil || got.Room != "!iris:chat.test" {
		t.Fatal("agent's own room stopped dispatching")
	}
	if got := findTurn(m, "$mentioned"); got == nil || got.Text != "summarise this" || got.Room != team {
		t.Fatalf("mention not dispatched or not stripped: %+v", got)
	}
}

// A bare command in a room several fleet bots share is refused with a pointer instead of acting
// in every bot at once; an addressed command, !stop, a single-bot room and the agent's own
// provisioned room are unaffected.
func TestSharedRoomCommandsRequireAddressing(t *testing.T) {
	c, s := testStore(t)
	team, solo := "!team:chat.test", "!solo:chat.test"
	s.Update(func(st *State) error {
		st.Members["iris"].Rooms[team] = &Room{}
		st.Members["iris"].Rooms[solo] = &Room{}
		st.Members["zed"] = &Member{ID: "zed", Enabled: true, Room: "!zed:chat.test", Rooms: map[string]*Room{team: {}}, Sessions: map[string]map[string]*Session{}}
		return nil
	})
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, syncRooms(map[string]any{
			"!iris:chat.test": map[string]any{"timeline": map[string]any{"events": []any{message("$own", "@admin:chat.test", "!menu", "")}}},
			team:              map[string]any{"timeline": map[string]any{"events": []any{message("$bare", "@admin:chat.test", "!menu", ""), message("$stop", "@admin:chat.test", "!stop", ""), addressed("$talk", "!menu")}}},
			solo:              map[string]any{"timeline": map[string]any{"events": []any{message("$lone", "@admin:chat.test", "!menu", "")}}},
		}))
	})
	if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	m := s.View().Members["iris"]
	if findTurn(m, "$bare") != nil {
		t.Fatal("ambiguous command dispatched in a shared room")
	}
	if findTurn(m, "$stop") == nil || findTurn(m, "$talk") == nil || findTurn(m, "$lone") == nil || findTurn(m, "$own") == nil {
		t.Fatalf("addressed command, !stop, single-bot room or own room refused: %+v", m.Turns)
	}
	if len(m.Outbox) != 1 || m.Outbox[0].ID != "$bare-ambiguous" || m.Outbox[0].Room != team || !strings.Contains(m.Outbox[0].Text, "@agent_iris:chat.test") {
		t.Fatalf("ambiguous command did not get one addressed explanation: %+v", m.Outbox)
	}
}

// A message addressed to another agent of the fleet belongs to that agent: this bot stays silent
// instead of answering it or handing another agent's addressing to Hermes, while its own
// addressing and ordinary prose that merely names another agent still work.
// Element displays an agent's short name, not its internal agent_ Matrix localpart.
// Display-name commands must route only to their selected agent in shared rooms.
func TestSharedRoomDisplayNameAddressing(t *testing.T) {
	c, s := testStore(t)
	team := "!team:chat.test"
	s.Update(func(st *State) error {
		st.Members["iris"].Rooms[team] = &Room{Mention: true}
		st.Members["zed"] = &Member{ID: "zed", Enabled: true, Room: "!zed:chat.test", Rooms: map[string]*Room{team: {}}, Sessions: map[string]map[string]*Session{}}
		return nil
	})
	mention := func(id, text, target string) map[string]any {
		ev := message(id, "@admin:chat.test", text, "")
		ev["content"].(map[string]any)["m.mentions"] = map[string]any{"user_ids": []string{target}}
		return ev
	}
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, syncRooms(map[string]any{
			team: map[string]any{"timeline": map[string]any{"events": []any{
				mention("$display", "Iris !menu", "@agent_iris:chat.test"),
				mention("$display-colon", "Iris: !status", "@agent_iris:chat.test"),
				message("$at-display", "@admin:chat.test", "@Iris !help", ""),
				message("$colon", "@admin:chat.test", "iris: !models", ""),
				mention("$other", "Zed: !menu", "@agent_zed:chat.test"),
				message("$prose", "@admin:chat.test", "Iris is a useful name", ""),
				message("$prefix", "@admin:chat.test", "iris2: !stop", ""),
			}}},
		}))
	})
	if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	m := s.View().Members["iris"]
	for id, command := range map[string]string{
		"$display": "!menu", "$display-colon": "!status", "$at-display": "!help", "$colon": "!models",
	} {
		if got := findTurn(m, id); got == nil || got.Text != command || got.Kind != "command" {
			t.Fatalf("display-name address was not routed to Iris: id=%s turn=%+v", id, got)
		}
	}
	for _, id := range []string{"$other", "$prose", "$prefix"} {
		if got := findTurn(m, id); got != nil {
			t.Fatalf("message for another agent or plain prose triggered Iris: %+v", got)
		}
	}
}

func TestMessagesAddressedToAnotherAgentAreIgnored(t *testing.T) {
	c, s := testStore(t)
	team := "!team:chat.test"
	s.Update(func(st *State) error {
		// Reply mode off: without this rule every message here would be dispatched.
		st.Members["iris"].Rooms[team] = &Room{}
		st.Members["zed"] = &Member{ID: "zed", Enabled: true, Room: "!zed:chat.test", Rooms: map[string]*Room{team: {}}, Sessions: map[string]map[string]*Session{}}
		return nil
	})
	// A mention of both agents still opens with the other agent's handle, so the addressee wins.
	both := message("$both", "@admin:chat.test", "agent_zed: !menu", "")
	both["content"].(map[string]any)["m.mentions"] = map[string]any{"user_ids": []string{"@agent_zed:chat.test", "@agent_iris:chat.test"}}
	mention := message("$mention", "@admin:chat.test", "summarise this", "")
	mention["content"].(map[string]any)["m.mentions"] = map[string]any{"user_ids": []string{"@agent_zed:chat.test"}}
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, syncRooms(map[string]any{team: map[string]any{"timeline": map[string]any{"events": []any{
			// Element's rendered mention: the localpart with its own colon trail.
			message("$handle", "@admin:chat.test", "agent_zed: !menu", ""),
			message("$full", "@admin:chat.test", "@agent_zed:chat.test: !menu", ""),
			both,
			mention,
			message("$prose", "@admin:chat.test", "agent_zed:chat.test is slow today, summarise this", ""),
			addressed("$own", "!menu"),
		}}}}))
	})
	if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	m := s.View().Members["iris"]
	for _, id := range []string{"$handle", "$full", "$both", "$mention"} {
		if findTurn(m, id) != nil {
			t.Fatalf("%s is another agent's message but was dispatched: %+v", id, findTurn(m, id))
		}
	}
	if len(m.Outbox) != 0 {
		t.Fatalf("ignoring another agent's message must be silent: %+v", m.Outbox)
	}
	if findTurn(m, "$prose") == nil {
		t.Fatal("prose that only names another agent must still reach this agent")
	}
	if findTurn(m, "$own") == nil {
		t.Fatal("message addressed to this agent was ignored")
	}
}

func TestRequireMentionCommandTogglesPerRoom(t *testing.T) {
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) { replyJSON(w, map[string]any{}) })
	room := "!team:chat.test"
	s.Update(func(st *State) error {
		st.Members["iris"].Rooms[room] = &Room{Mention: true}
		st.Members["iris"].Turns = []*Turn{{ID: "toggle", Room: room, Kind: "command", Phase: "queued", Text: "!require_mention off"}}
		return nil
	})
	engine.command(context.Background(), "iris", "toggle")
	m := s.View().Members["iris"]
	if m.Rooms[room].Mention || !strings.Contains(m.Outbox[0].Text, "no longer required") || m.Outbox[0].Room != room {
		t.Fatal("mention requirement not cleared or reply misrouted")
	}
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: "on", Room: room, Kind: "command", Phase: "queued", Text: "!require_mention on"})
		return nil
	})
	engine.command(context.Background(), "iris", "on")
	if !s.View().Members["iris"].Rooms[room].Mention {
		t.Fatal("mention requirement not restored")
	}
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: "bogus", Room: room, Kind: "command", Phase: "queued", Text: "!require_mention maybe"})
		return nil
	})
	engine.command(context.Background(), "iris", "bogus")
	if !strings.Contains(s.View().Members["iris"].Outbox[2].Text, "Usage:") {
		t.Fatal("invalid toggle accepted")
	}
}

// A gap in a room the operator created must not wedge every other room's sync.
func TestForeignRoomGapDoesNotWedgeSync(t *testing.T) {
	c, s := testStore(t)
	room := "!team:chat.test"
	s.Update(func(st *State) error { st.Members["iris"].Rooms[room] = &Room{LastEvent: "$gone"}; return nil })
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			replyJSON(w, map[string]any{"chunk": []any{}, "end": "end"})
			return
		}
		replyJSON(w, syncRooms(map[string]any{room: map[string]any{"timeline": map[string]any{"events": []any{message("$new", "@admin:chat.test", "hello", "")}, "limited": true, "prev_batch": "gap"}}}))
	})
	if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	m := s.View().Members["iris"]
	if findTurn(m, "$new") == nil || m.Rooms[room].LastEvent != "$new" {
		t.Fatal("foreign room history gap wedged the agent")
	}
}

// Replies for a room the bot has left are dropped instead of blocking the outbox head.
func TestRepliesToLeftRoomsAreDropped(t *testing.T) {
	c, s := testStore(t)
	var sent atomic.Int32
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		replyJSON(w, map[string]string{"event_id": "$sent"})
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		addNotice(m, "gone", "!gone:chat.test", "", "undeliverable")
		addNotice(m, "live", m.Room, "", "delivered")
		return nil
	})
	if err := mx.sendNext(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	m := s.View().Members["iris"]
	if sent.Load() != 0 || len(m.Outbox) != 1 || m.Outbox[0].ID != "live" {
		t.Fatalf("undeliverable reply not dropped: sent=%d outbox=%d", sent.Load(), len(m.Outbox))
	}
}

// State version 1 is upgraded in place: the single room becomes the provisioned room's cursor
// and its session map, without touching identities or history.
func TestStateUpgradeFromVersionOne(t *testing.T) {
	c := testConfig(t)
	os.MkdirAll(filepath.Join(c.Data, "controller"), 0700)
	v1 := map[string]any{
		"Version": 1, "ServerName": "chat.test", "PublicURL": "https://chat.test", "Admin": "admin",
		"Members": map[string]any{"iris": map[string]any{
			"ID": "iris", "Dir": "/members/iris", "Project": "iris", "Service": "hermes", "Enabled": true,
			"Room": "!iris:chat.test", "Since": "old-sync", "LastEvent": "$old", "LastActivity": time.Now(),
			"Sessions": map[string]any{"room": map[string]any{"ID": "matrix_room_session", "Seeded": true}, "$a": map[string]any{"ID": "matrix_thread_session"}},
			"Turns":    []any{map[string]any{"ID": "t1", "Thread": "$a", "Phase": "running", "RunID": "r1"}},
			"Outbox":   []any{map[string]any{"ID": "t1-reply", "Thread": "$a", "Text": "answer"}},
		}},
	}
	if err := os.WriteFile(filepath.Join(c.Data, "controller", "state.json"), jsonBytes(v1), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	m := s.View().Members["iris"]
	if m.Rooms["!iris:chat.test"] == nil || m.Rooms["!iris:chat.test"].LastEvent != "$old" || m.Rooms["!iris:chat.test"].Mention {
		t.Fatal("room cursor not migrated")
	}
	if m.Turns[0].Room != "!iris:chat.test" || m.Outbox[0].Room != "!iris:chat.test" || m.Since != "old-sync" {
		t.Fatal("turn/notice room not defaulted")
	}
	if m.Sessions["!iris:chat.test"]["room"].ID != "matrix_room_session" || m.Sessions["!iris:chat.test"]["$a"].ID != "matrix_thread_session" {
		t.Fatal("session map not migrated")
	}
	raw, _ := os.ReadFile(s.path)
	s.Close()
	if !strings.Contains(string(raw), `"Version":`+strconv.Itoa(stateVersion)) {
		t.Fatal("upgraded state was not persisted")
	}
	again, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.View().Members["iris"].Rooms["!iris:chat.test"].LastEvent != "$old" {
		t.Fatal("upgraded state did not reload")
	}
}

func TestUnknownStateVersionIsRefused(t *testing.T) {
	c := testConfig(t)
	os.MkdirAll(filepath.Join(c.Data, "controller"), 0700)
	if err := os.WriteFile(filepath.Join(c.Data, "controller", "state.json"), []byte(`{"Version":99,"Members":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(c); err == nil {
		t.Fatal("unknown state version accepted")
	}
}

// Invitations are reconciled on every start, because an incrementing sync only offers an
// invite relative to its cursor. Reconciliation must not advance the stored cursor.
func TestStartupInviteReconciliation(t *testing.T) {
	c, s := testStore(t)
	var joined []string
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/join/") {
			joined = append(joined, strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/join/"))
			replyJSON(w, map[string]string{"room_id": "joined"})
			return
		}
		body := syncInvites("!team:chat.test")
		invites := body["rooms"].(map[string]any)["invite"].(map[string]any)
		invites["!botted:chat.test"] = map[string]any{"invite_state": map[string]any{"events": []any{inviteEvent("@agent_other:chat.test")}}}
		replyJSON(w, body)
	})
	if err := mx.reconcileInvites(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	if len(joined) != 1 || joined[0] != "!team:chat.test" {
		t.Fatalf("startup reconciliation wrong: %v", joined)
	}
	if m := s.View().Members["iris"]; m.Since != "old-sync" {
		t.Fatal("startup reconciliation advanced the sync cursor")
	}
}

// stateEvent builds one room state event for a sync state block.
func stateEvent(typ string, content map[string]any) map[string]any {
	return map[string]any{"type": typ, "state_key": "", "sender": "@admin:chat.test", "content": content}
}

// joinedState is a joined room with a state block and an empty timeline.
func joinedState(events ...any) map[string]any {
	return map[string]any{"timeline": map[string]any{"events": []any{}}, "state": map[string]any{"events": events}}
}

// An agent greets a room once, when it is invited into it: not again on later syncs, not in its
// own provisioned room, and not in a space. The greeting names the room and explains the reply
// mode through the shipped command.
func TestJoinIntroPostedOnceForInvitedRooms(t *testing.T) {
	c, s := testStore(t)
	team, unnamed, space := "!team:chat.test", "!unnamed:chat.test", "!space:chat.test"
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/state/m.room.name/") {
			w.WriteHeader(404)
			return
		}
		replyJSON(w, syncRooms(map[string]any{
			"!iris:chat.test": joinedState(stateEvent("m.room.create", map[string]any{}), stateEvent("m.room.name", map[string]any{"name": "Iris"})),
			team:              joinedState(stateEvent("m.room.create", map[string]any{}), stateEvent("m.room.name", map[string]any{"name": "Finance2"})),
			unnamed:           joinedState(stateEvent("m.room.create", map[string]any{})),
			space:             joinedState(stateEvent("m.room.create", map[string]any{"type": "m.space"})),
		}))
	})
	for i := 0; i < 2; i++ {
		if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
			t.Fatal(err)
		}
	}
	m := s.View().Members["iris"]
	if len(m.Outbox) != 2 {
		t.Fatalf("expected exactly one greeting per invited room, got %+v", m.Outbox)
	}
	greetings := map[string]string{}
	for _, n := range m.Outbox {
		greetings[n.ID] = n.Text
	}
	named := greetings[team+"-joined"]
	if !strings.Contains(named, "Happy to join Finance2.") || !strings.Contains(named, "only when mentioned") || !strings.Contains(named, "!require_mention off") {
		t.Fatalf("greeting does not explain the reply mode: %q", named)
	}
	if got := greetings[unnamed+"-joined"]; !strings.Contains(got, "Happy to join this room.") {
		t.Fatalf("unnamed room not labelled: %q", got)
	}
	if m.Rooms[space] == nil {
		t.Fatal("space not tracked")
	}
}

func TestRoomLabelSanitisesUntrustedNames(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Finance2", "Finance2"},
		{"two\nlines\tand  spaces", "two lines and spaces"},
		{"", "this room"},
		{"  \n\t ", "this room"},
		{strings.Repeat("é", 100), strings.Repeat("é", 32)},
	}
	for _, tc := range cases {
		got := roomLabel(tc.in)
		if got != tc.want {
			t.Fatalf("roomLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if len(got) > 64 || !utf8.ValidString(got) || strings.ContainsAny(got, "\n\r\t") {
			t.Fatalf("roomLabel(%q) produced unusable output %q", tc.in, got)
		}
	}
}

// One unwritable room must not silence every other room: a blocked entry is skipped, its own
// room keeps FIFO order, and !resend brings it back.
func TestBlockedReplyDoesNotStarveOtherRooms(t *testing.T) {
	c, s := testStore(t)
	var sent []string
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.URL.Path)
		replyJSON(w, map[string]string{"event_id": "$sent"})
	})
	blocked, other := "!blocked:chat.test", "!other:chat.test"
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Rooms[blocked], m.Rooms[other] = &Room{}, &Room{}
		addNotice(m, "stuck", blocked, "", "undeliverable")
		addNotice(m, "behind", blocked, "", "queued behind it")
		addNotice(m, "fine", other, "", "deliverable")
		m.Outbox[0].Attempts = 8
		return nil
	})
	if err := mx.sendNext(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], other) {
		t.Fatalf("blocked room starved another room: %v", sent)
	}
	m := s.View().Members["iris"]
	if len(m.Outbox) != 2 || m.Outbox[0].ID != "stuck" || m.Outbox[1].ID != "behind" {
		t.Fatalf("blocked entry was not preserved in order: %+v", m.Outbox)
	}
	s.Update(func(st *State) error {
		for _, n := range st.Members["iris"].Outbox {
			n.Attempts, n.Next = 0, time.Time{}
		}
		return nil
	})
	if err := mx.sendNext(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || !strings.Contains(sent[1], blocked) {
		t.Fatalf("retry did not resume the blocked room first: %v", sent)
	}
	if m = s.View().Members["iris"]; len(m.Outbox) != 1 || m.Outbox[0].ID != "behind" {
		t.Fatalf("same-room order broken: %+v", m.Outbox)
	}
}
