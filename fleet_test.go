package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeFleetFixture(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"name": name, "services": map[string]any{"hermes": map[string]any{"image": "hermes:test", "network_mode": "host", "volumes": []any{map[string]any{"type": "bind", "source": filepath.Join(dir, "data"), "target": "/opt/data"}}}}}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), jsonBytes(config), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := configureEnv(filepath.Join(dir, "data", ".env"), "127.0.0.1", 8642); err != nil {
		t.Fatal(err)
	}
}

func fleetFixture(t *testing.T) (*Engine, context.Context, *atomic.Int32, string) {
	t.Helper()
	c, s := testStore(t)
	root := t.TempDir()
	c.FleetDir, c.TemplateDir = filepath.Join(root, "agents"), filepath.Join(root, "templates")
	if err := fleetStorage(c); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(c.FleetDir, "iris")
	writeFleetFixture(t, dir, "iris")
	if err := s.Update(func(st *State) error {
		a := st.Members["iris"]
		a.Dir, a.Home, a.Files, a.Managed, a.Name, a.Network, a.Port = dir, filepath.Join(dir, "data"), []string{filepath.Join(dir, "compose.yaml")}, true, "iris", "host", 8642
		st.Members[fleetID] = &Member{ID: fleetID, Name: "Fleet", Controller: true, Enabled: true, Room: "!fleet:chat.test", Rooms: map[string]*Room{"!fleet:chat.test": {}}, Sessions: map[string]map[string]*Session{}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "commands")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nfile=''\nwhile [ \"$#\" -gt 0 ]; do\n case \"$1\" in\n --file) file=\"$2\"; shift 2;;\n config) cat \"$file\"; exit $?;;\n *) shift;;\n esac\ndone\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	deactivations := &atomic.Int32{}
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/deactivate/"):
			var body struct{ Erase bool }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Erase || r.Header.Get("Authorization") != "Bearer private-admin-token" {
				t.Error("unsafe deactivation request")
			}
			deactivations.Add(1)
			replyJSON(w, map[string]string{"id_server_unbind_result": "success"})
		case strings.HasSuffix(p, "/sync"):
			<-r.Context().Done()
		case strings.HasSuffix(p, "/whoami"):
			name := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer token-")
			if name == r.Header.Get("Authorization") {
				name = "iris"
			}
			replyJSON(w, map[string]string{"user_id": "@" + name + ":chat.test"})
		case strings.HasSuffix(p, "/login"):
			var body struct{ Identifier struct{ User string } }
			_ = json.NewDecoder(r.Body).Decode(&body)
			replyJSON(w, map[string]string{"access_token": "token-" + body.Identifier.User, "user_id": "@" + body.Identifier.User + ":chat.test"})
		case strings.Contains(p, "/directory/room/"):
			http.Error(w, "missing", 404)
		case strings.HasSuffix(p, "/createRoom"):
			var body struct {
				Alias string `json:"room_alias_name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			replyJSON(w, map[string]string{"room_id": "!" + strings.TrimPrefix(body.Alias, "hermes_") + ":chat.test"})
		case strings.Contains(p, "/state/org.hermes.fleet/"):
			parts := strings.Split(p, "/")
			id := strings.TrimSuffix(strings.TrimPrefix(parts[5], "!"), ":chat.test")
			replyJSON(w, map[string]string{"agent": id, "server": "chat.test"})
		case strings.Contains(p, "/state/m.room.member/"):
			replyJSON(w, map[string]string{"membership": "join"})
		case strings.Contains(p, "/state/m.room.power_levels/"):
			replyJSON(w, map[string]any{"users": map[string]int{"@admin:chat.test": 100}})
		default:
			replyJSON(w, map[string]any{})
		}
	})
	mx.secretPath = filepath.Join(c.Data, "secrets", "accounts.json")
	key, err := configureEnv(filepath.Join(dir, "data", ".env"), "127.0.0.1", 8642)
	if err != nil {
		t.Fatal(err)
	}
	mx.secrets.APIKeys["iris"] = key
	mx.secrets.Accounts["agent_"+fleetID] = &Credential{User: "@agent_" + fleetID + ":chat.test", Token: "token-agent_" + fleetID}
	ds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { replyJSON(w, []any{}) }))
	t.Cleanup(ds.Close)
	u, _ := url.Parse(ds.URL)
	e := &Engine{c: c, s: s, mx: mx, docker: &Docker{client: &http.Client{Transport: rewriteTransport{u, http.DefaultTransport}}}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); e.wg.Wait() })
	return e, ctx, deactivations, log
}

func fleetTestCommand(t *testing.T, e *Engine, ctx context.Context, text string) string {
	t.Helper()
	id := randomSecret()[:12]
	if err := e.s.Update(func(st *State) error {
		m := st.Members[fleetID]
		m.Turns = append(m.Turns, &Turn{ID: id, Room: m.Room, Sender: "@admin:chat.test", Text: text, Kind: "command", Phase: "queued"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.fleetCommand(ctx, id)
	return id
}

// fleetTestText queues the operator's plain reply, as used to answer a name prompt.
func fleetTestText(t *testing.T, e *Engine, ctx context.Context, text string) string {
	t.Helper()
	id := randomSecret()[:12]
	if err := e.s.Update(func(st *State) error {
		m := st.Members[fleetID]
		m.Turns = append(m.Turns, &Turn{ID: id, Room: m.Room, Sender: "@admin:chat.test", Text: text, Phase: "queued"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.fleetCommand(ctx, id)
	return id
}

// fleetTap selects a menu option by action, exercising the real menu path.
func fleetTap(t *testing.T, e *Engine, ctx context.Context, menuAction, menuValue, optionAction, optionValue string) {
	t.Helper()
	id := randomSecret()[:12]
	if err := e.s.Update(func(st *State) error {
		m := st.Members[fleetID]
		fleetMenu(st, &Turn{ID: id, Sender: "@admin:chat.test", Room: m.Room}, menuAction, menuValue, e.c.TemplateDir)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	menu := e.s.View().Members[fleetID].Menus[id+"-fleet-menu"]
	if menu == nil {
		t.Fatal("menu was not posted")
	}
	key := ""
	for _, o := range menu.Options {
		if o.Action == optionAction && (optionValue == "" || o.Value == optionValue) {
			key = o.Key
		}
	}
	if key == "" {
		t.Fatalf("menu option %q not found", optionAction)
	}
	turn := randomSecret()[:12]
	if err := e.s.Update(func(st *State) error {
		m := st.Members[fleetID]
		m.Turns = append(m.Turns, &Turn{ID: turn, Room: m.Room, Sender: "@admin:chat.test", Text: "!menu-choice", Kind: "command", Phase: "queued", MenuID: id + "-fleet-menu", Choice: key})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.fleetCommand(ctx, turn)
}

func completeFleetTestOperation(t *testing.T, e *Engine, ctx context.Context) {
	t.Helper()
	for i := 0; i < 8 && e.s.View().FleetOperation != nil; i++ {
		e.fleetStep(ctx)
		if op := e.s.View().FleetOperation; op != nil && op.Error != "" {
			t.Fatalf("operation held: %s", op.Error)
		}
	}
	if e.s.View().FleetOperation != nil {
		t.Fatal("operation did not complete")
	}
}

func TestFleetAgentMenuIsFlatAndIdentityBound(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	id := fleetTestCommand(t, e, ctx, "!fleet")
	m := e.s.View().Members[fleetID]
	if len(m.Menus[id+"-fleet-menu"].Options) != 3 {
		t.Fatal("unexpected home menu")
	}
	_ = e.s.Update(func(st *State) error {
		turn := &Turn{ID: "flat", Sender: "@admin:chat.test", Room: st.Members[fleetID].Room}
		fleetMenu(st, turn, "agent", "iris", e.c.TemplateDir)
		return nil
	})
	menu := e.s.View().Members[fleetID].Menus["flat-fleet-menu"]
	if len(menu.Options) != 8 {
		t.Fatal("agent actions not all on one menu")
	}
	for _, o := range menu.Options {
		if strings.Contains(o.Label, "More") || o.Key == "" {
			t.Fatal("nested or unusable action")
		}
		if o.Action != "home" && o.Value != "iris" {
			t.Fatal("action not bound to agent identity")
		}
	}
}

func TestFleetDeleteConfirmationDrainAndRetire(t *testing.T) {
	e, ctx, deactivations, log := fleetFixture(t)
	dir := e.s.View().Members["iris"].Dir
	fleetTestCommand(t, e, ctx, "!fleet delete iris")
	cf := e.s.View().FleetConfirmation
	if cf == nil || e.s.View().FleetOperation != nil {
		t.Fatal("delete did not require confirmation")
	}
	fleetTestCommand(t, e, ctx, "!fleet confirm-delete iris wrong")
	if e.s.View().FleetOperation != nil {
		t.Fatal("wrong confirmation accepted")
	}
	_ = e.s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "admitted", Phase: "held"}}
		return nil
	})
	fleetTestCommand(t, e, ctx, "!fleet confirm-delete iris "+cf.Code)
	e.fleetStep(ctx)
	if e.s.View().FleetOperation.Stage != "waiting" {
		t.Fatal("held work did not prevent deletion")
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "down") {
		t.Fatal("busy agent was stopped")
	}
	_ = e.s.Update(func(st *State) error { st.Members["iris"].Turns[0].Phase = "done"; return nil })
	completeFleetTestOperation(t, e, ctx)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("managed data not erased")
	}
	if deactivations.Load() != 1 || !e.s.View().Members["iris"].Deleted || e.mx.key("iris") != "" {
		t.Fatal("deleted agent not retired")
	}
	if _, err := e.mx.account(ctx, "agent_iris", false); err == nil {
		t.Fatal("retired Matrix identity was reactivated")
	}
	if fleetMember(e.s.View(), "iris") != nil {
		t.Fatal("deleted agent still in active inventory")
	}
	if e.s.View().Members["iris"].Room != "!iris:chat.test" {
		t.Fatal("historical Matrix mapping lost")
	}
	if b, err := os.ReadFile(log); err != nil || !strings.Contains(string(b), "down --remove-orphans --timeout 60") {
		t.Fatal("project-owned orphan containers were not included in shutdown")
	}
}

func TestFleetCancelWaitingPauseAndEnable(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	fleetTestCommand(t, e, ctx, "!fleet pause iris")
	if !e.s.View().Members["iris"].Draining {
		t.Fatal("new work not blocked")
	}
	fleetTestCommand(t, e, ctx, "!fleet cancel")
	if e.s.View().FleetOperation != nil || e.s.View().Members["iris"].Draining {
		t.Fatal("waiting cancellation failed")
	}
	fleetTestCommand(t, e, ctx, "!fleet pause iris")
	completeFleetTestOperation(t, e, ctx)
	if !e.s.View().Members["iris"].Paused {
		t.Fatal("pause not persisted")
	}
	if _, _, err := e.ensure(ctx, "iris", "!iris:chat.test"); err == nil {
		t.Fatal("paused agent woke")
	}
	fleetTestCommand(t, e, ctx, "!fleet enable iris")
	if e.s.View().Members["iris"].Paused {
		t.Fatal("enable failed")
	}
}

func TestFleetFullCopySnapshotAndCleanCreation(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	if err := e.s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.DefaultModel, m.DefaultProvider = "copied-model", "copied-provider"
		m.Sessions[m.Room] = map[string]*Session{"room": {ID: "copied-current-session", Title: "Current conversation"}, "chat:old": {ID: "copied-older-session", Title: "Older conversation"}}
		m.Sessions["!shared:chat.test"] = map[string]*Session{"room": {ID: "copied-shared-session", Title: "Shared conversation"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	source := e.s.View().Members["iris"]
	for _, name := range []string{"memory.md", "history.json", "provider-token"} {
		if err := os.WriteFile(filepath.Join(source.Home, name), []byte("private-fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fleetTestCommand(t, e, ctx, "!fleet copy iris to nova")
	completeFleetTestOperation(t, e, ctx)
	copy := fleetMember(e.s.View(), "nova")
	if copy == nil || copy.Paused || copy.ID == "nova" || copy.Room == source.Room || copy.Port == source.Port {
		t.Fatal("copy did not become ready to work with an independent identity")
	}
	for _, name := range []string{"memory.md", "history.json", "provider-token"} {
		if b, err := os.ReadFile(filepath.Join(copy.Home, name)); err != nil || string(b) != "private-fixture" {
			t.Fatal("full copy omitted agent data")
		}
	}
	if e.mx.key(copy.ID) == e.mx.key(source.ID) || e.mx.key(copy.ID) == "" {
		t.Fatal("operational API credential was copied")
	}
	if copy.DefaultModel != "copied-model" || copy.DefaultProvider != "copied-provider" || len(copy.Sessions[copy.Room]) != 3 || copy.Sessions[copy.Room]["room"].ID != "copied-current-session" {
		t.Fatal("full copy lost bridge defaults or conversation selection/history")
	}
	fleetTestCommand(t, e, ctx, "!fleet save-template iris as iris-snapshot")
	completeFleetTestOperation(t, e, ctx)
	tpl, err := readFleetTemplate(e.c.TemplateDir, "iris-snapshot")
	if err != nil || tpl.Kind != "Full snapshot" {
		t.Fatal("full snapshot not listed")
	}
	fleetTestCommand(t, e, ctx, "!fleet create lyra from iris-snapshot")
	completeFleetTestOperation(t, e, ctx)
	if m := fleetMember(e.s.View(), "lyra"); m == nil || m.Paused || m.DefaultModel != "copied-model" || len(m.Sessions[m.Room]) != 3 {
		t.Fatal("snapshot-created agent not ready to work")
	}
	clean := filepath.Join(e.c.TemplateDir, "basic")
	writeFleetFixture(t, clean, "basic")
	if err := os.WriteFile(filepath.Join(clean, "fleet-template.json"), jsonBytes(FleetTemplate{Kind: "Clean"}), 0600); err != nil {
		t.Fatal(err)
	}
	fleetTestCommand(t, e, ctx, "!fleet create fresh from basic")
	completeFleetTestOperation(t, e, ctx)
	if m := fleetMember(e.s.View(), "fresh"); m == nil || m.Paused {
		t.Fatal("clean-created agent not available")
	}
}

func TestFleetCopyFailureRetryAndPublishedRecovery(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	source := e.s.View().Members["iris"]
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(source.Home, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	fleetTestCommand(t, e, ctx, "!fleet copy iris to nova")
	for i := 0; i < 3; i++ {
		e.fleetStep(ctx)
	}
	op := e.s.View().FleetOperation
	if op == nil || op.Error == "" || !e.s.View().Members["iris"].Draining {
		t.Fatal("unsafe copy did not hold source")
	}
	if _, err := os.Stat(filepath.Join(e.c.FleetDir, "nova")); !os.IsNotExist(err) {
		t.Fatal("incomplete copy was published")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	fleetTestCommand(t, e, ctx, "!fleet retry")
	e.fleetStep(ctx)
	op = e.s.View().FleetOperation
	if op.Stage != "provisioning" || op.Error != "" {
		t.Fatal("copy retry failed")
	}
	// Simulate a crash after directory publication, before the operation stage was saved.
	if err := e.s.Update(func(st *State) error { st.FleetOperation.Stage = "copying"; return nil }); err != nil {
		t.Fatal(err)
	}
	e.fleetStep(ctx)
	completeFleetTestOperation(t, e, ctx)
	if fleetMember(e.s.View(), "nova") == nil {
		t.Fatal("published copy was not recovered")
	}
}

func TestFleetNameReuseWarnsAndUsesFreshIdentity(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	fleetTestCommand(t, e, ctx, "!fleet save-template iris as snapshot")
	completeFleetTestOperation(t, e, ctx)
	fleetTestCommand(t, e, ctx, "!fleet delete iris")
	cf := e.s.View().FleetConfirmation
	fleetTestCommand(t, e, ctx, "!fleet confirm-delete iris "+cf.Code)
	completeFleetTestOperation(t, e, ctx)
	fleetTestCommand(t, e, ctx, "!fleet create iris from snapshot")
	cf = e.s.View().FleetConfirmation
	if cf == nil || cf.Action != "create" || e.s.View().FleetOperation != nil {
		t.Fatal("name reuse did not require warning confirmation")
	}
	fleetTestCommand(t, e, ctx, "!fleet confirm-create iris "+cf.Code)
	completeFleetTestOperation(t, e, ctx)
	m := fleetMember(e.s.View(), "iris")
	if m == nil || m.ID == "iris" || m.Room == "!iris:chat.test" {
		t.Fatal("deleted identity/history reused")
	}
}

func TestFleetStorageBoundaries(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, outside, link, filepath.Join(root, "..", "victim")} {
		if err := deleteFleetTree(root, path); err == nil {
			t.Fatal("unsafe deletion accepted")
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("external data touched")
	}
	config := map[string]any{"services": map[string]any{"hermes": map[string]any{"volumes": []any{map[string]any{"type": "bind", "source": outside, "target": "/external"}}}}}
	if validateFleetCompose(config, root) == nil {
		t.Fatal("external writable storage accepted")
	}
	config["services"].(map[string]any)["hermes"].(map[string]any)["volumes"].([]any)[0].(map[string]any)["read_only"] = true
	if err := validateFleetCompose(config, root); err != nil {
		t.Fatal("operator-approved read-only mount rejected")
	}
	if validateFleetMounts("/fleet/iris", "1 2 0:1 / /fleet rw - ext4 /dev/test rw\n") != nil {
		t.Fatal("parent mount rejected")
	}
	for _, mount := range []string{"/fleet/iris", "/fleet/iris/data"} {
		if validateFleetMounts("/fleet/iris", "1 2 0:1 / "+mount+" rw - ext4 /dev/test rw\n") == nil {
			t.Fatal("independent agent mount accepted")
		}
	}
}

func TestFleetSyncOperatorOnlyAndPauseAdmission(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	e.mx.Base = httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, syncRooms(map[string]any{
			"!fleet:chat.test": map[string]any{"timeline": map[string]any{"events": []any{
				message("foreign", "@someone:chat.test", "!fleet delete iris", ""),
				message("owner", "@admin:chat.test", "!fleet agents", ""),
			}}},
			"!iris:chat.test": map[string]any{"timeline": map[string]any{"events": []any{message("wrong-room", "@admin:chat.test", "!fleet delete iris", "")}}},
		}))
	}))
	if err := e.mx.syncOnce(ctx, e.s, fleetID); err != nil {
		t.Fatal(err)
	}
	m := e.s.View().Members[fleetID]
	if len(m.Turns) != 1 || m.Turns[0].ID != "owner" || m.Since != "next-sync" {
		t.Fatal("Fleet room sender/room allowlist failed")
	}
	if err := e.mx.syncOnce(ctx, e.s, fleetID); err != nil {
		t.Fatal(err)
	}
	if len(e.s.View().Members[fleetID].Turns) != 1 {
		t.Fatal("fleet command replay admitted")
	}
	_ = e.s.Update(func(st *State) error { st.Members["iris"].Paused = true; return nil })
	e.mx.Base = httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, syncResponse([]any{message("paused-text", "@admin:chat.test", "hello", "")}, false))
	}))
	if err := e.mx.syncOnce(ctx, e.s, "iris"); err != nil {
		t.Fatal(err)
	}
	if pending(e.s.View().Members["iris"]) != 0 {
		t.Fatal("paused agent admitted new work")
	}
}

func httptestServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

func TestFleetDrainingChecksAuthenticatedActivity(t *testing.T) {
	e, ctx, _, log := fleetFixture(t)
	var busy atomic.Bool
	busy.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if busy.Load() {
			replyJSON(w, map[string]any{"gateway_busy": true})
		} else {
			fakeHealth(w)
		}
	}))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	_ = e.s.Update(func(st *State) error { st.Members["iris"].Port = port; return nil })
	ds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := e.s.View().Members["iris"]
		labels := map[string]string{"com.docker.compose.project": "iris", "com.docker.compose.service": "hermes", "com.docker.compose.project.working_dir": a.Dir}
		if strings.Contains(r.URL.Path, "/containers/c/") {
			replyJSON(w, map[string]any{"Config": map[string]any{"Labels": labels}, "HostConfig": map[string]string{"NetworkMode": "host"}})
		} else {
			replyJSON(w, []any{map[string]any{"Id": "c", "Labels": labels}})
		}
	}))
	t.Cleanup(ds.Close)
	du, _ := url.Parse(ds.URL)
	e.docker.client = &http.Client{Transport: rewriteTransport{du, http.DefaultTransport}}
	fleetTestCommand(t, e, ctx, "!fleet shutdown iris")
	e.fleetStep(ctx)
	if e.s.View().FleetOperation.Stage != "waiting" {
		t.Fatal("incomplete/busy authenticated health allowed shutdown")
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), " down ") {
		t.Fatal("shutdown occurred while busy")
	}
	busy.Store(false)
	e.fleetStep(ctx)
	if e.s.View().FleetOperation.Stage != "stopping" {
		t.Fatal("confirmed idle agent did not leave draining")
	}
}

func TestFleetExpiredConfirmationAndStateRecovery(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	fleetTestCommand(t, e, ctx, "!fleet delete iris")
	cf := e.s.View().FleetConfirmation
	_ = e.s.Update(func(st *State) error { st.FleetConfirmation.Expires = time.Now().Add(-time.Second); return nil })
	fleetTestCommand(t, e, ctx, "!fleet confirm-delete iris "+cf.Code)
	if e.s.View().FleetOperation != nil {
		t.Fatal("expired delete confirmed")
	}
	fleetTestCommand(t, e, ctx, "!fleet pause iris")
	var restored State
	if _, err := loadState(jsonBytes(e.s.View()), &restored); err != nil || !restored.Members["iris"].Draining || !validFleetOperation(restored.FleetOperation, restored) {
		t.Fatal("operation cannot resume after state reload")
	}
	old := e.s.View()
	old.Version = 4
	if changed, err := loadState(jsonBytes(old), &restored); err != nil || !changed || restored.Version != stateVersion {
		t.Fatal("version 4 migration failed")
	}
}

func TestFleetReconcilePersistsPauseAndDoesNotResurrectDeletedDirectory(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	e.c.Dirs = []string{e.s.View().Members["iris"].Dir}
	fleetTestCommand(t, e, ctx, "!fleet pause iris")
	completeFleetTestOperation(t, e, ctx)
	if err := reconcileFleet(ctx, e.c, e.s, e.mx, e.docker); err != nil {
		t.Fatal(err)
	}
	if !e.s.View().Members["iris"].Paused {
		t.Fatal("startup reconciliation unpaused the agent")
	}
	fleetTestCommand(t, e, ctx, "!fleet delete iris")
	cf := e.s.View().FleetConfirmation
	fleetTestCommand(t, e, ctx, "!fleet confirm-delete iris "+cf.Code)
	e.fleetStep(ctx)
	e.fleetStep(ctx)
	if op := e.s.View().FleetOperation; op == nil || op.Stage != "deleting" {
		t.Fatal("delete did not reach durable destructive phase")
	}
	// Simulate a restart after filesystem deletion, before final controller state was saved.
	if err := deleteFleetTree(e.c.FleetDir, e.c.Dirs[0]); err != nil {
		t.Fatal(err)
	}
	if err := reconcileFleet(ctx, e.c, e.s, e.mx, e.docker); err != nil {
		t.Fatal(err)
	}
	completeFleetTestOperation(t, e, ctx)
	if err := reconcileFleet(ctx, e.c, e.s, e.mx, e.docker); err != nil {
		t.Fatal(err)
	}
	if fleetMember(e.s.View(), "iris") != nil {
		t.Fatal("stale HERMES_DIRS entry resurrected a deleted agent")
	}
}

func TestFleetNeverOverwritesUnownedDestination(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	dst := filepath.Join(e.c.FleetDir, "nova")
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dst, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	fleetTestCommand(t, e, ctx, "!fleet copy iris to nova")
	for i := 0; i < 3; i++ {
		e.fleetStep(ctx)
	}
	if op := e.s.View().FleetOperation; op == nil || op.Error == "" {
		t.Fatal("unowned destination accepted")
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "keep" {
		t.Fatal("unowned destination modified")
	}
}

func TestFleetNamePromptCopy(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	fleetTap(t, e, ctx, "agent", "iris", promptCopy, "")
	prompt := e.s.View().FleetPrompt
	if prompt == nil || prompt.Action != promptCopy || prompt.Source != "iris" || prompt.Owner != "@admin:chat.test" {
		t.Fatal("copy did not request a name")
	}
	fleetTestText(t, e, ctx, "nova")
	if e.s.View().FleetPrompt != nil {
		t.Fatal("answered prompt stayed pending")
	}
	op := e.s.View().FleetOperation
	if op == nil || op.Action != "copy" || op.Name != "nova" {
		t.Fatal("plain reply did not start the copy")
	}
	completeFleetTestOperation(t, e, ctx)
	copy := fleetMember(e.s.View(), "nova")
	if copy == nil || copy.Paused {
		t.Fatal("copied agent missing or not ready to work")
	}
}

func TestFleetNamePromptCreateFromTemplate(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	clean := filepath.Join(e.c.TemplateDir, "basic")
	writeFleetFixture(t, clean, "basic")
	if err := os.WriteFile(filepath.Join(clean, "fleet-template.json"), jsonBytes(FleetTemplate{Kind: "Clean"}), 0600); err != nil {
		t.Fatal(err)
	}
	fleetTap(t, e, ctx, "templates", "", "template", "basic")
	prompt := e.s.View().FleetPrompt
	if prompt == nil || prompt.Action != promptCreate || prompt.Source != "basic" {
		t.Fatal("template did not request an agent name")
	}
	fleetTestText(t, e, ctx, "lyra")
	op := e.s.View().FleetOperation
	if op == nil || op.Action != "create" || op.Name != "lyra" {
		t.Fatal("plain reply did not start the creation")
	}
	completeFleetTestOperation(t, e, ctx)
	created := fleetMember(e.s.View(), "lyra")
	if created == nil || created.Paused {
		t.Fatal("clean-created agent missing or not available")
	}
}

func TestFleetNamePromptInvalidNameKeepsPrompt(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	fleetTap(t, e, ctx, "agent", "iris", promptCopy, "")
	fleetTestText(t, e, ctx, "Not A Valid Name")
	if e.s.View().FleetPrompt == nil {
		t.Fatal("invalid name discarded the prompt")
	}
	if e.s.View().FleetOperation != nil {
		t.Fatal("invalid name started an operation")
	}
	m := e.s.View().Members[fleetID]
	if !strings.Contains(strings.ToLower(m.Outbox[len(m.Outbox)-1].Text), "not a valid agent name") {
		t.Fatal("invalid name was not explained")
	}
	fleetTestText(t, e, ctx, "nova")
	if e.s.View().FleetOperation == nil {
		t.Fatal("prompt did not recover after a valid name")
	}
}

func TestFleetNamePromptExpiryAndCancel(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	fleetTap(t, e, ctx, "agent", "iris", promptCopy, "")
	_ = e.s.Update(func(st *State) error {
		st.FleetPrompt.Expires = time.Now().Add(-time.Second)
		return nil
	})
	fleetTestText(t, e, ctx, "nova")
	if e.s.View().FleetOperation != nil || e.s.View().FleetPrompt != nil {
		t.Fatal("expired prompt was answered")
	}
	m := e.s.View().Members[fleetID]
	if !strings.Contains(m.Outbox[len(m.Outbox)-1].Text, "expired") {
		t.Fatal("expiry was not explained")
	}
	fleetTap(t, e, ctx, "agent", "iris", promptCopy, "")
	fleetTestCommand(t, e, ctx, "!fleet cancel")
	if e.s.View().FleetPrompt != nil {
		t.Fatal("cancel did not clear the prompt")
	}
}

func TestFleetNamePromptRefusedWhileConfirmationPending(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	fleetTestCommand(t, e, ctx, "!fleet delete iris")
	if e.s.View().FleetConfirmation == nil {
		t.Fatal("delete confirmation missing")
	}
	fleetTap(t, e, ctx, "agent", "iris", promptCopy, "")
	if e.s.View().FleetPrompt != nil {
		t.Fatal("prompt started while a confirmation was pending")
	}
}

func TestFleetPlainTextOnlyAdmittedWhilePromptPending(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	e.mx.Base = httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, syncRooms(map[string]any{
			"!fleet:chat.test": map[string]any{"timeline": map[string]any{"events": []any{
				message("plain", "@admin:chat.test", "just chatting", ""),
			}}},
		}))
	}))
	if err := e.mx.syncOnce(ctx, e.s, fleetID); err != nil {
		t.Fatal(err)
	}
	if len(e.s.View().Members[fleetID].Turns) != 0 {
		t.Fatal("plain text admitted without a pending prompt")
	}
	_ = e.s.Update(func(st *State) error {
		st.FleetPrompt = &FleetPrompt{Action: promptCopy, Source: "iris", Owner: "@admin:chat.test", Expires: time.Now().Add(time.Minute)}
		return nil
	})
	if err := e.mx.syncOnce(ctx, e.s, fleetID); err != nil {
		t.Fatal(err)
	}
	turns := e.s.View().Members[fleetID].Turns
	if len(turns) != 1 || turns[0].Text != "just chatting" || turns[0].Kind != "" {
		t.Fatal("plain reply not admitted for a pending prompt")
	}
}

func TestFleetRejectsForgedOperatorAndWrongRoom(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	for i, turn := range []*Turn{
		{Sender: "@someone:chat.test", Room: "!fleet:chat.test"},
		{Sender: "@admin:chat.test", Room: "!other:chat.test"},
		{Sender: "@admin:chat.test", Room: "!fleet:chat.test", Thread: "$thread"},
	} {
		turn.ID = "forged-" + strconv.Itoa(i)
		turn.Text = "!fleet delete iris"
		turn.Kind = "command"
		turn.Phase = "queued"
		if err := e.s.Update(func(st *State) error { st.Members[fleetID].Turns = append(st.Members[fleetID].Turns, turn); return nil }); err != nil {
			t.Fatal(err)
		}
		e.fleetCommand(ctx, turn.ID)
	}
	if e.s.View().FleetConfirmation != nil || e.s.View().FleetOperation != nil {
		t.Fatal("unauthorized fleet operation accepted")
	}
}

func TestFleetRestoresRunningSourceAndManualStartActivity(t *testing.T) {
	e, ctx, _, log := fleetFixture(t)
	a := e.s.View().Members["iris"]
	marker := filepath.Join(a.Dir, ".test-running")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// Fake Compose changes only a private fixture marker, never real containers.
	bin := filepath.Dir(log)
	script := "#!/bin/sh\nfile=''\ndir=''\nwhile [ \"$#\" -gt 0 ]; do\n case \"$1\" in\n --file) file=\"$2\";shift 2;;\n --project-directory) dir=\"$2\";shift 2;;\n config) cat \"$file\";exit $?;;\n down) rm -f \"$dir/.test-running\";exit $?;;\n up) touch \"$dir/.test-running\";exit $?;;\n *) shift;;\n esac\ndone\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			fakeCapabilities(w)
		} else {
			fakeHealth(w)
		}
	}))
	t.Cleanup(hs.Close)
	hu, _ := url.Parse(hs.URL)
	port, _ := strconv.Atoi(hu.Port())
	if err := e.s.Update(func(st *State) error { st.Members["iris"].Port = port; return nil }); err != nil {
		t.Fatal(err)
	}
	ds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		labels := map[string]string{"com.docker.compose.project": "iris", "com.docker.compose.service": "hermes", "com.docker.compose.project.working_dir": a.Dir}
		if strings.Contains(r.URL.Path, "/containers/c/") {
			replyJSON(w, map[string]any{"Config": map[string]any{"Labels": labels}, "HostConfig": map[string]string{"NetworkMode": "host"}})
			return
		}
		if _, err := os.Stat(marker); err == nil {
			replyJSON(w, []any{map[string]any{"Id": "c", "Labels": labels}})
		} else {
			replyJSON(w, []any{})
		}
	}))
	t.Cleanup(ds.Close)
	du, _ := url.Parse(ds.URL)
	e.docker.client = &http.Client{Transport: rewriteTransport{du, http.DefaultTransport}}
	fleetTestCommand(t, e, ctx, "!fleet copy iris to nova")
	completeFleetTestOperation(t, e, ctx)
	if _, err := os.Stat(marker); err != nil || e.s.View().Members["iris"].Draining {
		t.Fatal("running source was not restored")
	}
	if _, err := os.Stat(filepath.Join(e.c.FleetDir, "nova", ".test-running")); !os.IsNotExist(err) {
		t.Fatal("source was copied without first stopping")
	}
	fleetTestCommand(t, e, ctx, "!fleet shutdown iris")
	completeFleetTestOperation(t, e, ctx)
	if err := e.s.Update(func(st *State) error { st.Members["iris"].LastActivity = time.Now().Add(-time.Hour); return nil }); err != nil {
		t.Fatal(err)
	}
	fleetTestCommand(t, e, ctx, "!fleet start iris")
	completeFleetTestOperation(t, e, ctx)
	if _, err := os.Stat(marker); err != nil || time.Since(e.s.View().Members["iris"].LastActivity) > time.Minute {
		t.Fatal("manual start did not refresh idle timer")
	}
}

func TestFleetDeactivationFailureStillRetiresCredentials(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	e.mx.Base = httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 503) }))
	fleetTestCommand(t, e, ctx, "!fleet delete iris")
	cf := e.s.View().FleetConfirmation
	fleetTestCommand(t, e, ctx, "!fleet confirm-delete iris "+cf.Code)
	completeFleetTestOperation(t, e, ctx)
	if e.mx.key("iris") != "" || e.mx.cred("agent_iris").Token != "" {
		t.Fatal("retired credentials retained")
	}
	if _, err := e.mx.account(ctx, "agent_iris", false); err == nil {
		t.Fatal("failed remote deactivation allowed bridge recreation")
	}
	m := e.s.View().Members[fleetID]
	if !strings.Contains(m.Outbox[len(m.Outbox)-1].Text, "could not be confirmed") {
		t.Fatal("deactivation failure not disclosed")
	}
}

func TestFleetNeverJoinsInvitedRooms(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	var joins atomic.Int32
	e.mx.Base = httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/join/") {
			joins.Add(1)
			replyJSON(w, map[string]any{})
			return
		}
		invite := map[string]any{"type": "m.room.member", "sender": "@admin:chat.test", "state_key": "@agent_" + fleetID + ":chat.test", "content": map[string]string{"membership": "invite"}}
		replyJSON(w, map[string]any{"next_batch": "fleet-next", "rooms": map[string]any{"invite": map[string]any{"!other:chat.test": map[string]any{"invite_state": map[string]any{"events": []any{invite}}}}}})
	}))
	if err := e.mx.syncOnce(ctx, e.s, fleetID); err != nil {
		t.Fatal(err)
	}
	if err := e.mx.reconcileInvites(ctx, e.s, fleetID); err != nil {
		t.Fatal(err)
	}
	if joins.Load() != 0 {
		t.Fatal("Fleet controller joined another room")
	}
}

func TestFleetRoomGetsOneInitialMenu(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	if err := e.s.Update(func(st *State) error { st.Members[fleetID].Room = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := e.mx.provision(ctx, e.s, fleetID); err != nil {
			t.Fatal(err)
		}
	}
	m := e.s.View().Members[fleetID]
	if menu := m.Menus["fleet-welcome-fleet-menu"]; menu == nil || len(menu.Options) != 3 || menu.Owner != "@admin:chat.test" {
		t.Fatal("Fleet room lacks initial operator menu")
	}
	if len(m.Outbox) != 1 {
		t.Fatal("startup reconciliation duplicated the welcome menu")
	}
}
