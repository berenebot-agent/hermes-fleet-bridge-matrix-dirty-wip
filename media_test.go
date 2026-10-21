package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mediaFixture(t *testing.T) (Config, *Store, *Engine) {
	t.Helper()
	c, s := testStore(t)
	home := t.TempDir()
	if err := s.Update(func(st *State) error { st.Members["iris"].Home = home; return nil }); err != nil {
		t.Fatal(err)
	}
	e := &Engine{c: c, s: s, agents: map[string]*agentRuntime{"iris": {busy: map[string]bool{}}}}
	return c, s, e
}

func mediaEvent(id, kind, body, filename, uri string) map[string]any {
	return map[string]any{"event_id": id, "sender": "@admin:chat.test", "type": "m.room.message", "content": map[string]any{"msgtype": kind, "body": body, "filename": filename, "url": uri, "info": map[string]any{"size": 4, "mimetype": "image/png"}}}
}

func TestMediaSyncDurableCaptionAndRejection(t *testing.T) {
	c, s, e := mediaFixture(t)
	image := mediaEvent("$image", "m.image", "What is this?", "photo.png", "mxc://chat.test/image")
	voice := mediaEvent("$voice", "m.audio", "voice.ogg", "", "mxc://chat.test/voice")
	voice["content"].(map[string]any)["org.matrix.msc3245.voice"] = map[string]any{}
	encrypted := mediaEvent("$encrypted", "m.file", "private.pdf", "", "mxc://chat.test/private")
	encrypted["content"].(map[string]any)["file"] = map[string]any{"url": "mxc://chat.test/private"}
	bot := mediaEvent("$bot", "m.file", "bot.txt", "", "mxc://chat.test/bot")
	bot["sender"] = "@agent_other:chat.test"
	command := mediaEvent("$caption-command", "m.file", "!stop", "code.txt", "mxc://chat.test/code")
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, syncResponse([]any{image, voice, encrypted, bot, command}, false))
	})
	e.mx = mx
	for range 2 {
		if err := mx.syncOnce(context.Background(), s, "iris"); err != nil {
			t.Fatal(err)
		}
	}
	m := s.View().Members["iris"]
	if m.Since != "next-sync" || len(m.Turns) != 4 {
		t.Fatalf("cursor/dedup: %q %d", m.Since, len(m.Turns))
	}
	if v := findTurn(m, "$image"); v.Text != "What is this?" || v.Media.Name != "photo.png" || v.Media.Ready {
		t.Fatal("lost caption or prematurely downloaded")
	}
	if v := findTurn(m, "$voice"); v.Media.Kind != "voice" || v.Text != "" {
		t.Fatal("voice marker or filename confused with prompt")
	}
	if findTurn(m, "$encrypted").Phase != "done" || len(m.Outbox) != 1 {
		t.Fatal("encrypted upload not visibly refused")
	}
	if v := findTurn(m, "$caption-command"); v.Kind != "" || v.Text != "!stop" {
		t.Fatal("caption became bridge control")
	}
	var restored State
	if _, err := loadState(jsonBytes(s.View()), &restored); err != nil || findTurn(restored.Members["iris"], "$image").Media.Source != "mxc://chat.test/image" {
		t.Fatal("media did not survive state reload")
	}
}

func TestMediaReferencesAndFilenames(t *testing.T) {
	for _, uri := range []string{"https://evil.test/a", "mxc://evil.test/a", "mxc://user@chat.test/a", "mxc://chat.test/a/b", "mxc://chat.test/%2e%2e", "mxc://chat.test/a?redirect=https://evil.test", "mxc://chat.test/a#x", "mxc://chat.test/%2fetc"} {
		if _, err := mediaDownloadPath(uri, "chat.test"); err == nil {
			t.Errorf("accepted %q", uri)
		}
	}
	if p, err := mediaDownloadPath("mxc://chat.test/abc", "chat.test"); err != nil || !strings.Contains(p, "allow_remote=false") {
		t.Fatal("valid local media refused")
	}
	if got := displayFilename("../../bad\nname.txt"); got != "badname.txt" {
		t.Fatal(got)
	}
}

func TestMediaDownloadRestartAndIntegrity(t *testing.T) {
	c, s, e := mediaFixture(t)
	var downloads atomic.Int32
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v1/media/download/chat.test/doc" || r.URL.Query().Get("allow_remote") != "false" || r.Header.Get("Authorization") != "Bearer private-bot-token" {
			t.Error("unsafe media download request")
		}
		downloads.Add(1)
		w.Write([]byte("hello"))
	})
	e.mx = mx
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "$file", Phase: "preparing", Media: &Attachment{Source: "mxc://chat.test/doc", Name: "doc.txt", Kind: "m.file"}}}
		return nil
	})
	for range 2 {
		if err := e.prepareMedia(context.Background(), "iris", "$file"); err != nil {
			t.Fatal(err)
		}
	}
	v := findTurn(s.View().Members["iris"], "$file")
	if downloads.Load() != 1 || !v.Media.Ready || v.Media.Size != 5 {
		t.Fatal("download repeated or metadata lost")
	}
	name := filepath.Join(s.View().Members["iris"].Home, "matrix-media", "incoming", stableID(v.ID), "file")
	os.WriteFile(name, []byte("other"), 0600)
	if err := e.prepareMedia(context.Background(), "iris", "$file"); err == nil {
		t.Fatal("changed staged bytes accepted")
	}
}

func TestMediaWriteLimitsAndUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, _, err := writeMedia(r, "file", strings.NewReader("12345"), 4); err == nil {
		t.Fatal("oversized attachment accepted")
	}
	if _, err := r.Stat("file"); !os.IsNotExist(err) {
		t.Fatal("partial output was committed")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("secret"), 0600)
	os.Symlink(outside, filepath.Join(dir, "symlink"))
	os.Link(outside, filepath.Join(dir, "hardlink"))
	os.Mkdir(filepath.Join(dir, "directory"), 0700)
	for _, name := range []string{"symlink", "hardlink", "directory"} {
		if f, err := regularMedia(r, name); err == nil {
			f.Close()
			t.Errorf("accepted %s", name)
		}
	}
	os.Symlink(t.TempDir(), filepath.Join(dir, "unsafe-dir"))
	if sub, err := mediaDir(r, "unsafe-dir", false); err == nil {
		sub.Close()
		t.Fatal("followed unsafe media directory")
	}
}

func TestMediaQuotaRefusesWithoutDownload(t *testing.T) {
	c, s, e := mediaFixture(t)
	var downloads atomic.Int32
	e.mx = fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) { downloads.Add(1); w.Write([]byte("unwanted")) })
	m := s.View().Members["iris"]
	r, err := memberMedia(m)
	if err != nil {
		t.Fatal(err)
	}
	f, err := r.OpenFile("full", os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(maxAgentMedia); err != nil {
		t.Fatal(err)
	}
	f.Close()
	r.Close()
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "quota", Phase: "preparing", Media: &Attachment{Kind: "m.file", Name: "doc.txt", Source: "mxc://chat.test/doc"}}}
		return nil
	})
	if err = e.prepareMedia(context.Background(), "iris", "quota"); err == nil || downloads.Load() != 0 {
		t.Fatal("quota-full media was downloaded")
	}
}

func TestMediaStateMigrationAndHeldRetention(t *testing.T) {
	_, s, e := mediaFixture(t)
	old := s.View()
	old.Version = 3
	var restored State
	changed, err := loadState(jsonBytes(old), &restored)
	if err != nil || !changed || restored.Version != stateVersion || restored.Members["iris"].Room != old.Members["iris"].Room {
		t.Fatal("version 3 identity not preserved")
	}
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "held", Phase: "held", Created: time.Now().Add(-30 * 24 * time.Hour)}}
		return nil
	})
	m := s.View().Members["iris"]
	dir := filepath.Join(m.Home, "matrix-media", "incoming", stableID("held"))
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "file"), []byte("pinned"), 0600)
	e.cleanMedia("iris")
	if _, err := os.Stat(filepath.Join(dir, "file")); err != nil {
		t.Fatal("held turn media removed")
	}
}

func TestOutputManagedSecretRefused(t *testing.T) {
	c, s, e := mediaFixture(t)
	e.mx = fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected network access") })
	m := s.View().Members["iris"]
	turn := &Turn{ID: "secret"}
	dir := filepath.Join(m.Home, "matrix-media", "outgoing", stableID(turn.ID))
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("token=private-bot-token"), 0600)
	if _, files, err := e.outputMedia(m, turn, fileMarker+"file.txt"); err == nil || len(files) != 0 {
		t.Fatal("managed credential was eligible for upload")
	}
}

func TestMediaOutputSnapshotRetryAndCleanup(t *testing.T) {
	c, s, e := mediaFixture(t)
	m := s.View().Members["iris"]
	turn := &Turn{ID: "$output", Phase: "running", Created: time.Now()}
	dir := filepath.Join(m.Home, "matrix-media", "outgoing", stableID(turn.ID))
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("original"), 0600)
	text, files, err := e.outputMedia(m, turn, "Done\nMATRIX_FILE: result.txt")
	if err != nil || text != "Done" || len(files) != 1 {
		t.Fatalf("snapshot failed: %v", err)
	}
	// A retry must reuse the committed snapshot, not the changed source.
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("changed"), 0600)
	_, again, err := e.outputMedia(m, turn, "MATRIX_FILE: result.txt")
	if err != nil || files[0].SHA256 != again[0].SHA256 {
		t.Fatal("snapshot changed on recovery")
	}
	var uploads, sends atomic.Int32
	var txn string
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/upload") {
			uploads.Add(1)
			b, _ := io.ReadAll(r.Body)
			if string(b) != "original" {
				t.Error("uploaded mutated bytes")
			}
			replyJSON(w, map[string]string{"content_uri": "mxc://chat.test/uploaded"})
			return
		}
		if sends.Add(1) == 1 {
			txn = r.URL.Path
			w.WriteHeader(503)
			return
		}
		if txn != r.URL.Path {
			t.Error("transaction ID changed")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["msgtype"] != "m.file" || body["url"] != "mxc://chat.test/uploaded" {
			t.Error("bad attachment message")
		}
		replyJSON(w, map[string]string{"event_id": "$sent"})
	})
	e.mx = mx
	s.Update(func(st *State) error {
		v := st.Members["iris"]
		turn.Phase = "done"
		turn.Completed = time.Now().Add(-8 * 24 * time.Hour)
		v.Turns = []*Turn{turn}
		v.Outbox = []*Notice{{ID: "file", Room: v.Room, TurnID: turn.ID, Media: files[0]}}
		return nil
	})
	if err := mx.sendNext(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	e.cleanMedia("iris")
	if findTurn(s.View().Members["iris"], turn.ID).MediaCleaned {
		t.Fatal("pending delivery was garbage collected")
	}
	// Reload through the real store validator, then retry the persisted media URI.
	s.Close()
	reloaded, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	s = reloaded
	e.s = s
	defer s.Close()
	s.Update(func(st *State) error { st.Members["iris"].Outbox[0].Next = time.Time{}; return nil })
	if err := mx.sendNext(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	if uploads.Load() != 1 || sends.Load() != 2 || len(s.View().Members["iris"].Outbox) != 0 {
		t.Fatal("upload repeated or delivery incomplete")
	}
	e.cleanMedia("iris")
	if !findTurn(s.View().Members["iris"], turn.ID).MediaCleaned {
		t.Fatal("expired terminal media not cleaned")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("outgoing files retained beyond expiry")
	}
}

func TestMediaDeliverySkipsBlockedRoomHead(t *testing.T) {
	c, s, e := mediaFixture(t)
	m := s.View().Members["iris"]
	turn := &Turn{ID: "$other-file", Room: "!other:chat.test", Phase: "done", Completed: time.Now()}
	dir := filepath.Join(m.Home, "matrix-media", "outgoing", stableID(turn.ID))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("result"), 0600); err != nil {
		t.Fatal(err)
	}
	_, files, err := e.outputMedia(m, turn, "MATRIX_FILE: report.txt")
	if err != nil || len(files) != 1 {
		t.Fatalf("failed to create outgoing media: %v", err)
	}
	var uploads, sends atomic.Int32
	mx := fakeMatrix(t, c, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/upload") {
			uploads.Add(1)
			replyJSON(w, map[string]string{"content_uri": "mxc://chat.test/other-file"})
			return
		}
		sends.Add(1)
		if !strings.Contains(r.URL.Path, "!other:chat.test") {
			t.Errorf("delivery went to the wrong room: %s", r.URL.Path)
		}
		if sends.Load() == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		replyJSON(w, map[string]string{"event_id": "$sent"})
	})
	s.Update(func(st *State) error {
		member := st.Members["iris"]
		member.Rooms[turn.Room] = &Room{}
		member.Turns = append(member.Turns, turn)
		member.Outbox = []*Notice{
			{ID: "blocked", Room: member.Room, Text: "stuck", Attempts: 8},
			{ID: "media", Room: turn.Room, TurnID: turn.ID, Media: files[0]},
		}
		return nil
	})
	if err := mx.sendNext(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	got := s.View().Members["iris"].Outbox
	if len(got) != 2 || got[0].ID != "blocked" || got[1].ID != "media" ||
		got[1].Media.Uploaded != "mxc://chat.test/other-file" || got[1].Attempts != 1 {
		t.Fatalf("non-head upload progress was not saved: %+v", got)
	}
	if err := s.Update(func(st *State) error {
		st.Members["iris"].Outbox[1].Next = time.Time{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := mx.sendNext(context.Background(), s, "iris"); err != nil {
		t.Fatal(err)
	}
	got = s.View().Members["iris"].Outbox
	if uploads.Load() != 1 || sends.Load() != 2 || len(got) != 1 || got[0].ID != "blocked" {
		t.Fatalf("non-head file was redelivered or blocked: uploads=%d sends=%d remaining=%+v", uploads.Load(), sends.Load(), got)
	}
}

func TestOutputReferenceConfinement(t *testing.T) {
	_, s, e := mediaFixture(t)
	m := s.View().Members["iris"]
	turn := &Turn{ID: "turn"}
	for _, name := range []string{"../.env", "/opt/data/.env", "sub/file", "..", "x\\y"} {
		if _, files, err := e.outputMedia(m, turn, fileMarker+name); err == nil || len(files) != 0 {
			t.Errorf("accepted %q", name)
		}
	}
	if _, _, err := e.outputMedia(m, turn, strings.Repeat("MATRIX_FILE: file\n", 6)); err == nil {
		t.Fatal("accepted more than five files")
	}
}

func TestVoiceTextAndControls(t *testing.T) {
	for _, pcm := range [][]byte{nil, make([]byte, 3200), make([]byte, voiceSeconds*pcmRate+2)} {
		if validateVoicePCM(pcm) == nil {
			t.Fatal("silence, empty, or overlong PCM accepted")
		}
	}
	for _, bad := range []string{"{}", "not json", `{"text":"  "}`} {
		if _, err := speechTranscript([]byte(bad)); err == nil {
			t.Fatal("accepted unusable transcript")
		}
	}
	text, err := speechTranscript([]byte(`{"text":"!approve request"}`))
	if err != nil {
		t.Fatal(err)
	}
	tn := &Turn{ID: "voice", Text: text, Media: &Attachment{Kind: "voice", Transcript: text}}
	if tn.Kind != "" || !strings.HasPrefix(mediaPrompt(tn, text), "!approve request\n") {
		t.Fatal("transcribed text became a bridge command")
	}
	pcm := []byte{1, 0, 2, 0}
	wav := pcmWave(pcm)
	if len(wav) != 48 || string(wav[:4]) != "RIFF" || !bytes.Equal(wav[44:], pcm) {
		t.Fatal("invalid WAV wrapper")
	}
}

func TestVoiceTranscriptNotice(t *testing.T) {
	m := &Member{ID: "iris", Room: "!room:chat.test"}
	tn := &Turn{ID: "turn-1", Room: "!other:chat.test", Text: "make a report", Media: &Attachment{Kind: "voice", Transcript: "make a report"}}
	addVoiceNotice(m, tn)
	if len(m.Outbox) != 1 {
		t.Fatal("voice transcript was not queued")
	}
	n := m.Outbox[0]
	if n.ID != "turn-1-voice" || n.Room != "!other:chat.test" || n.Thread != "" || !n.Plain {
		t.Fatalf("unexpected notice %+v", n)
	}
	if n.Text != "🎙️ Transcript sent to the agent:\nmake a report" {
		t.Fatalf("unexpected notice text %q", n.Text)
	}
	addVoiceNotice(m, tn)
	if len(m.Outbox) != 1 {
		t.Fatal("voice transcript was queued twice")
	}
	content := noticeContent("*not emphasis*", true, false)
	if _, ok := content["format"]; ok {
		t.Fatal("plain notice carried a formatted body")
	}
	if content["body"] != "*not emphasis*" {
		t.Fatalf("plain notice body was altered: %v", content["body"])
	}
	if body := noticeContent("*emphasis*", false, false)["formatted_body"]; body != "<i>emphasis</i>" {
		t.Fatalf("unexpected formatted body %q", body)
	}
}

func TestMediaDownloadDoesNotBlockStop(t *testing.T) {
	var requests atomic.Int32
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); fakeCapabilities(w) })
	s.Update(func(st *State) error {
		st.Members["iris"].Home = t.TempDir()
		st.Members["iris"].Turns = []*Turn{{ID: "slow", Phase: "queued", Media: &Attachment{Kind: "voice", Name: "voice.ogg", Source: "mxc://chat.test/slow"}}}
		return nil
	})
	started, release := make(chan struct{}), make(chan struct{})
	e.mx = fakeMatrix(t, e.c, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/media/download/") {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.Write([]byte("voice"))
			return
		}
		replyJSON(w, map[string]any{})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer func() { close(release); cancel(); e.wg.Wait() }()
	e.tick(ctx, "iris")
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("media download did not start")
	}
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: "stop-media", Kind: "command", Text: "!stop", Phase: "queued"})
		return nil
	})
	e.tick(ctx, "iris")
	await(t, func() bool {
		m := s.View().Members["iris"]
		return findTurn(m, "stop-media").Phase == "done" && findTurn(m, "slow").Phase == "done"
	})
	if requests.Load() != 0 {
		t.Fatal("cancelled media woke Hermes")
	}
}

func TestVoiceTurnUsesTextAPIAndQueuesOutputFile(t *testing.T) {
	var admitted atomic.Int32
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		case "/v1/runs":
			admitted.Add(1)
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			input, _ := body["input"].(string)
			if !strings.HasPrefix(input, "make a report\n") || !strings.Contains(input, "MATRIX_FILE:") || r.Header.Get("Idempotency-Key") == "" {
				t.Error("voice was not a normal idempotent text turn")
			}
			replyJSON(w, map[string]string{"run_id": "voice-run"})
		case "/v1/runs/voice-run":
			replyJSON(w, map[string]string{"run_id": "voice-run", "status": "completed", "output": "MATRIX_FILE: report.txt"})
		case "/v1/runs/voice-run/events":
			w.WriteHeader(404)
		default:
			t.Errorf("unexpected Hermes API path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	home := t.TempDir()
	data := []byte("audio")
	hash := sha256.Sum256(data)
	tn := &Turn{ID: "voice-report", Phase: "queued", Text: "make a report", Media: &Attachment{Kind: "voice", Ready: true, Transcript: "make a report", Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}}
	for _, kind := range []string{"incoming", "outgoing"} {
		os.MkdirAll(filepath.Join(home, "matrix-media", kind, stableID(tn.ID)), 0700)
	}
	os.WriteFile(filepath.Join(home, "matrix-media", "incoming", stableID(tn.ID), "file"), data, 0600)
	os.WriteFile(filepath.Join(home, "matrix-media", "outgoing", stableID(tn.ID), "report.txt"), []byte("report"), 0600)
	s.Update(func(st *State) error {
		st.Members["iris"].Home = home
		st.Members["iris"].Turns = []*Turn{tn}
		return nil
	})
	e.work(context.Background(), "iris", tn.ID)
	m := s.View().Members["iris"]
	if admitted.Load() != 1 || findTurn(m, tn.ID).Phase != "done" || len(m.Outbox) != 1 || m.Outbox[0].Media == nil || m.Outbox[0].TurnID != tn.ID {
		t.Fatal("voice run or attachment outbox incomplete")
	}
}

func TestStoppedMediaRunStillReconcilesAfterRestart(t *testing.T) {
	var stops atomic.Int32
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			fakeCapabilities(w)
		case "/health/detailed":
			fakeHealth(w)
		case "/v1/runs/existing/stop":
			stops.Add(1)
			replyJSON(w, map[string]any{})
		case "/v1/runs/existing":
			replyJSON(w, map[string]string{"run_id": "existing", "status": "cancelled"})
		case "/v1/runs/existing/events":
			w.WriteHeader(404)
		default:
			t.Error("unexpected admission or endpoint")
			w.WriteHeader(400)
		}
	})
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "stopped-media", Phase: "running", RunID: "existing", Stop: true, Submitted: time.Now(), Media: &Attachment{Kind: "voice"}}}
		return nil
	})
	e.work(context.Background(), "iris", "stopped-media")
	if stops.Load() != 1 || findTurn(s.View().Members["iris"], "stopped-media").Phase != "done" {
		t.Fatal("stop prevented recovery of an already admitted media run")
	}
}

func TestVoiceRuntime(t *testing.T) {
	if os.Getenv("HFB_SPEECH_SMOKE") != "1" {
		t.Skip("explicit real-runtime smoke only")
	}
	_, s, e := mediaFixture(t)
	m := s.View().Members["iris"]
	data, err := os.ReadFile("/samples/0.wav")
	if err != nil {
		t.Fatal(err)
	}
	opus, err := speechCommand(context.Background(), bytes.NewReader(data), maxAttachment, "ffmpeg", "-nostdin", "-loglevel", "error", "-i", "pipe:0", "-c:a", "libopus", "-f", "ogg", "pipe:1")
	if err != nil {
		t.Fatal(err)
	}
	for i, recording := range [][]byte{data, opus} {
		hash := sha256.Sum256(recording)
		tn := &Turn{ID: fmt.Sprintf("voice-smoke-%d", i), Phase: "preparing", Media: &Attachment{Kind: "voice", Ready: true, Size: int64(len(recording)), SHA256: hex.EncodeToString(hash[:])}}
		dir := filepath.Join(m.Home, "matrix-media", "incoming", stableID(tn.ID))
		os.MkdirAll(dir, 0700)
		os.WriteFile(filepath.Join(dir, "file"), recording, 0600)
		s.Update(func(st *State) error { st.Members["iris"].Turns = []*Turn{tn}; return nil })
		start := time.Now()
		if err = e.prepareMedia(context.Background(), "iris", tn.ID); err != nil {
			t.Fatal(err)
		}
		v := findTurn(s.View().Members["iris"], tn.ID)
		if !strings.Contains(strings.ToLower(v.Text), "old portrait") || v.Media.Transcript == "" {
			t.Fatal("real transcription did not become a durable text prompt")
		}
		t.Logf("voice sample format %d transcribed and persisted in %s", i, time.Since(start).Round(time.Millisecond))
		// Persisted transcripts must bypass all native work after recovery.
		originalPath := os.Getenv("PATH")
		t.Setenv("PATH", "/nonexistent")
		if err = e.prepareMedia(context.Background(), "iris", tn.ID); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", originalPath)
	}
}
