package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// A local key server exercises the actual SDK/Olm/Megolm and SQLite, not mock crypto.
type cryptoServer struct {
	mu       sync.Mutex
	keys     map[string]json.RawMessage
	otks     map[string]map[string]json.RawMessage
	cross    map[string]map[string]json.RawMessage
	toDevice map[string][]any
	timeline []any
	sends    []json.RawMessage
	failSend bool
	media    []byte
}

func newCryptoServer(t *testing.T) (*cryptoServer, *httptest.Server) {
	f := &cryptoServer{keys: map[string]json.RawMessage{}, otks: map[string]map[string]json.RawMessage{}, cross: map[string]map[string]json.RawMessage{}, toDevice: map[string][]any{}}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		user := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.HasSuffix(r.URL.Path, "/upload") && strings.Contains(r.URL.Path, "/media/") {
			if r.Header.Get("Content-Type") != "application/octet-stream" || r.URL.RawQuery != "" {
				t.Error("encrypted upload exposed filename or MIME")
			}
			f.media, _ = io.ReadAll(r.Body)
			replyJSON(w, map[string]string{"content_uri": "mxc://chat.test/encrypted-file"})
			return
		}
		if strings.Contains(r.URL.Path, "/media/download/") {
			w.Write(f.media)
			return
		}
		var body map[string]json.RawMessage
		if r.Method != "GET" {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("invalid request JSON on %s: %v", r.URL.Path, err)
			}
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/keys/upload"):
			if raw := body["device_keys"]; len(raw) > 0 {
				f.keys[user] = raw
			}
			var otks map[string]json.RawMessage
			_ = json.Unmarshal(body["one_time_keys"], &otks)
			if f.otks[user] == nil {
				f.otks[user] = map[string]json.RawMessage{}
			}
			for k, v := range otks {
				f.otks[user][k] = v
			}
			replyJSON(w, map[string]any{"one_time_key_counts": map[string]int{"signed_curve25519": len(f.otks[user])}})
		case strings.HasSuffix(r.URL.Path, "/keys/query"):
			devices := map[string]any{}
			for u, raw := range f.keys {
				devices[u] = map[string]any{"HERMES_FLEET_BRIDGE": raw}
			}
			resp := map[string]any{"device_keys": devices}
			for kind, users := range f.cross {
				resp[kind] = users
			}
			replyJSON(w, resp)
		case strings.HasSuffix(r.URL.Path, "/keys/device_signing/upload"):
			for _, kind := range []string{"master", "self_signing", "user_signing"} {
				if f.cross[kind+"_keys"] == nil {
					f.cross[kind+"_keys"] = map[string]json.RawMessage{}
				}
				f.cross[kind+"_keys"][user] = body[kind+"_key"]
			}
			replyJSON(w, map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/keys/signatures/upload"):
			var signed map[string]json.RawMessage
			_ = json.Unmarshal(body[user], &signed)
			if raw := signed["HERMES_FLEET_BRIDGE"]; len(raw) > 0 {
				var old, updated map[string]json.RawMessage
				_ = json.Unmarshal(f.keys[user], &old)
				_ = json.Unmarshal(raw, &updated)
				var sigs, newSigs map[string]map[string]string
				_ = json.Unmarshal(old["signatures"], &sigs)
				_ = json.Unmarshal(updated["signatures"], &newSigs)
				for u, values := range newSigs {
					if sigs[u] == nil {
						sigs[u] = map[string]string{}
					}
					for k, v := range values {
						sigs[u][k] = v
					}
				}
				old["signatures"] = jsonBytes(sigs)
				f.keys[user] = jsonBytes(old)
			}
			replyJSON(w, map[string]any{"failures": map[string]any{}})
		case strings.HasSuffix(r.URL.Path, "/keys/claim"):
			var requested map[string]map[string]string
			_ = json.Unmarshal(body["one_time_keys"], &requested)
			result := map[string]any{}
			for u := range requested {
				for key, val := range f.otks[u] {
					result[u] = map[string]any{"HERMES_FLEET_BRIDGE": map[string]any{key: val}}
					delete(f.otks[u], key)
					break
				}
			}
			replyJSON(w, map[string]any{"one_time_keys": result})
		case strings.Contains(r.URL.Path, "/sendToDevice/"):
			var messages map[string]map[string]json.RawMessage
			_ = json.Unmarshal(body["messages"], &messages)
			kind := strings.Split(strings.Split(r.URL.Path, "/sendToDevice/")[1], "/")[0]
			for u, devices := range messages {
				for _, content := range devices {
					f.toDevice[u] = append(f.toDevice[u], map[string]any{"type": kind, "sender": user, "content": content})
				}
			}
			replyJSON(w, map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/members"):
			var members []any
			for u := range f.keys {
				members = append(members, map[string]any{"type": "m.room.member", "state_key": u, "content": map[string]string{"membership": "join"}})
			}
			replyJSON(w, map[string]any{"chunk": members})
		case strings.Contains(r.URL.Path, "/state/m.room.encryption"):
			replyJSON(w, map[string]any{"algorithm": "m.megolm.v1.aes-sha2"})
		case strings.HasSuffix(r.URL.Path, "/sync"):
			resp := syncResponse(f.timeline, false)
			resp["to_device"] = map[string]any{"events": f.toDevice[user]}
			resp["device_one_time_keys_count"] = map[string]int{"signed_curve25519": len(f.otks[user])}
			f.toDevice[user] = nil
			f.timeline = nil
			replyJSON(w, resp)
		case strings.Contains(r.URL.Path, "/send/m.room.encrypted/"):
			f.sends = append(f.sends, jsonBytes(body))
			if f.failSend {
				f.failSend = false
				w.WriteHeader(503)
				replyJSON(w, map[string]any{"errcode": "M_UNKNOWN"})
				return
			}
			replyJSON(w, map[string]any{"event_id": "$encrypted-reply"})
		default:
			t.Errorf("unexpected crypto endpoint: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
			replyJSON(w, map[string]any{"errcode": "M_NOT_FOUND"})
		}
	}))
	t.Cleanup(s.Close)
	return f, s
}

func TestEncryptedRoundTripDelayedKeysRestartAndRetry(t *testing.T) {
	ctx := context.Background()
	c, store := testStore(t)
	f, server := newCryptoServer(t)
	bot := Credential{User: "@agent_iris:chat.test", Token: "@agent_iris:chat.test", Password: "test-only-password"}
	mx := &Matrix{Base: server.URL, c: c, crypto: map[string]*matrixCrypto{}, secrets: Secrets{Accounts: map[string]*Credential{"agent_iris": &bot}}}
	if err := mx.initCrypto(ctx, "iris", bot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mx.closeCrypto)
	if err := mx.ensureEncrypted(ctx, "iris", "!iris:chat.test"); err != nil {
		t.Fatal(err)
	}
	stateSync := jsonBytes(map[string]any{"rooms": map[string]any{"join": map[string]any{"!iris:chat.test": map[string]any{"state": map[string]any{"events": []any{
		map[string]any{"type": "m.room.history_visibility", "state_key": "", "content": map[string]string{"history_visibility": "joined"}},
		map[string]any{"type": "org.hermes.fleet", "state_key": "", "content": map[string]string{"agent": "iris"}},
	}}}}}})
	if err := mx.cryptoSync(ctx, "iris", stateSync, ""); err != nil {
		t.Fatal(err)
	}
	visibility, err := mx.cryptoFor("iris").client.StateStore.GetHistoryVisibility(ctx, "!iris:chat.test")
	if err != nil || visibility == nil || visibility.HistoryVisibility != event.HistoryVisibilityJoined {
		t.Fatal("crypto history-visibility state not persisted")
	}
	if err := store.Update(func(st *State) error {
		addNotice(st.Members["iris"], "welcome", "!iris:chat.test", "", "Welcome")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := mx.sendNext(ctx, store, "iris"); err != nil {
		t.Fatal(err)
	}
	if pending := store.View().Members["iris"].Outbox; len(pending) != 1 || pending[0].Attempts != 0 || len(pending[0].Ciphertext) != 0 {
		t.Fatal("pre-login welcome consumed retries or encrypted without a human device")
	}
	if err := store.Update(func(st *State) error { st.Members["iris"].Outbox = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	humanConfig := c
	humanConfig.Data = t.TempDir()
	human := &Matrix{Base: server.URL, c: humanConfig, crypto: map[string]*matrixCrypto{}}
	if err := human.initCrypto(ctx, "phone", Credential{User: "@admin:chat.test", Token: "@admin:chat.test", Password: "test-only-password"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(human.closeCrypto)
	phone := human.cryptoFor("phone")
	if _, err := phone.client.Members(ctx, "!iris:chat.test"); err != nil {
		t.Fatal(err)
	}
	if err := phone.client.StateStore.SetEncryptionEvent(ctx, "!iris:chat.test", &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}); err != nil {
		t.Fatal(err)
	}
	content, err := phone.helper.Encrypt(ctx, "!iris:chat.test", event.EventMessage, map[string]any{"msgtype": "m.text", "body": "encrypted hello"})
	if err != nil {
		t.Fatal("phone encryption:", err)
	}
	f.mu.Lock()
	keys := f.toDevice[bot.User]
	f.toDevice[bot.User] = nil
	f.timeline = []any{map[string]any{"type": "m.room.encrypted", "event_id": "$encrypted-input", "origin_server_ts": time.Now().UnixMilli(), "sender": "@admin:chat.test", "content": content}}
	f.mu.Unlock()
	if err := mx.syncOnce(ctx, store, "iris"); err != nil {
		t.Fatal(err)
	}
	if m := store.View().Members["iris"]; len(m.Encrypted) != 1 || len(m.Turns) != 0 || m.Since != "next-sync" {
		t.Fatal("missing keys lost ciphertext or dispatched plaintext")
	}
	// Restart both stores before the delayed Olm key delivery.
	mx.closeCrypto()
	mx.crypto = map[string]*matrixCrypto{}
	if err := mx.initCrypto(ctx, "iris", bot); err != nil {
		t.Fatal("restart:", err)
	}
	store.Close()
	store, err = openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	f.mu.Lock()
	f.toDevice[bot.User] = keys
	f.mu.Unlock()
	if err := mx.syncOnce(ctx, store, "iris"); err != nil {
		t.Fatal(err)
	}
	m := store.View().Members["iris"]
	if len(m.Encrypted) != 0 || len(m.Turns) != 1 || m.Turns[0].Text != "encrypted hello" {
		t.Fatal("delayed ciphertext was not recovered exactly once")
	}
	if err := store.Update(func(st *State) error {
		addNotice(st.Members["iris"], "reply", "!iris:chat.test", "", "encrypted answer")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failSend = true
	f.mu.Unlock()
	if err := mx.sendNext(ctx, store, "iris"); err != nil {
		t.Fatal(err)
	}
	// A lost send response must also survive a process restart with identical ciphertext.
	mx.closeCrypto()
	mx.crypto = map[string]*matrixCrypto{}
	if err := mx.initCrypto(ctx, "iris", bot); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *State) error { st.Members["iris"].Outbox[0].Next = time.Time{}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := mx.sendNext(ctx, store, "iris"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	sends := append([]json.RawMessage(nil), f.sends...)
	deviceEvents := f.toDevice["@admin:chat.test"]
	f.mu.Unlock()
	if len(sends) != 2 || string(sends[0]) != string(sends[1]) || strings.Contains(string(sends[0]), "encrypted answer") {
		t.Fatal("retry did not retain identical ciphertext")
	}
	if err := human.cryptoSync(ctx, "phone", jsonBytes(map[string]any{"to_device": map[string]any{"events": deviceEvents}}), ""); err != nil {
		t.Fatal(err)
	}
	var encrypted event.EncryptedEventContent
	if json.Unmarshal(sends[1], &encrypted) != nil {
		t.Fatal("invalid reply")
	}
	if encrypted.Algorithm != id.AlgorithmMegolmV1 {
		t.Fatal("reply is not Megolm")
	}
	raw := jsonBytes(map[string]any{"type": "m.room.encrypted", "event_id": "$reply", "origin_server_ts": time.Now().UnixMilli(), "sender": bot.User, "content": &encrypted})
	got, err := human.decryptEvent(ctx, "phone", "!iris:chat.test", raw)
	if err != nil || got.Content.Body != "encrypted answer" {
		t.Fatal("phone could not decrypt reply", err)
	}
	// Stage and send a real encrypted file, then download it through the incoming path.
	fileData := []byte("private attachment round trip")
	private, err := privateMedia(c, "iris")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := mediaDir(private, stableID("file-turn"), true)
	if err != nil {
		t.Fatal(err)
	}
	checksum, size, err := writeMedia(dir, "0", bytes.NewReader(fileData), maxAttachment)
	dir.Close()
	private.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(st *State) error {
		st.Members["iris"].Home = t.TempDir()
		st.Members["iris"].Outbox = []*Notice{{ID: "file-reply", Room: "!iris:chat.test", TurnID: "file-turn", Media: &Attachment{Name: "private.txt", MIME: "text/plain", Kind: "m.file", Ready: true, Size: size, SHA256: checksum}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = mx.sendNext(ctx, store, "iris"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	fileWire := append(json.RawMessage(nil), f.sends[len(f.sends)-1]...)
	delivered := f.toDevice["@admin:chat.test"]
	f.toDevice["@admin:chat.test"] = nil
	uploaded := append([]byte(nil), f.media...)
	f.mu.Unlock()
	if bytes.Equal(uploaded, fileData) {
		t.Fatal("media uploaded in plaintext")
	}
	if err = human.cryptoSync(ctx, "phone", jsonBytes(map[string]any{"to_device": map[string]any{"events": delivered}}), ""); err != nil {
		t.Fatal(err)
	}
	fileRaw := jsonBytes(map[string]any{"type": "m.room.encrypted", "event_id": "$file-reply", "origin_server_ts": time.Now().UnixMilli(), "sender": bot.User, "content": fileWire})
	fileEvent, err := human.decryptEvent(ctx, "phone", "!iris:chat.test", fileRaw)
	if err != nil {
		t.Fatal("encrypted file message", err)
	}
	file := incomingMedia(fileEvent)
	if file.Error != "" || file.File == nil {
		t.Fatal("encrypted file metadata lost")
	}
	if err = store.Update(func(st *State) error {
		st.Members["iris"].Turns = append(st.Members["iris"].Turns, &Turn{ID: "$file-input", Phase: "preparing", Media: file})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{c: c, s: store, mx: mx}
	if err = engine.prepareMedia(ctx, "iris", "$file-input"); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(store.View().Members["iris"].Home, "matrix-media", "incoming", stableID("$file-input"), "file")
	plain, err := os.ReadFile(staged)
	if err != nil || !bytes.Equal(plain, fileData) {
		t.Fatal("encrypted file download did not produce original bytes")
	}
	// Wrong-room ciphertext and a forged sender must never reach Hermes.
	if _, err = human.decryptEvent(ctx, "phone", "!wrong:chat.test", raw); err == nil {
		t.Fatal("cross-room ciphertext accepted")
	}
	forged := jsonBytes(map[string]any{"type": "m.room.encrypted", "event_id": "$forged", "origin_server_ts": time.Now().UnixMilli(), "sender": "@unknown:chat.test", "content": &encrypted})
	if _, err = human.decryptEvent(ctx, "phone", "!iris:chat.test", forged); err == nil {
		t.Fatal("forged/replayed ciphertext accepted")
	}
	// Production does not accept a plaintext downgrade, even in its known room.
	before := len(store.View().Members["iris"].Turns)
	f.mu.Lock()
	f.timeline = []any{message("$plaintext", "@admin:chat.test", "!stop", "")}
	f.mu.Unlock()
	if err = mx.syncOnce(ctx, store, "iris"); err != nil {
		t.Fatal(err)
	}
	if len(store.View().Members["iris"].Turns) != before {
		t.Fatal("plaintext downgrade dispatched")
	}
	leaveSync := jsonBytes(map[string]any{"rooms": map[string]any{"join": map[string]any{"!iris:chat.test": map[string]any{"timeline": map[string]any{"events": []any{
		map[string]any{"type": "m.room.member", "state_key": "@admin:chat.test", "content": map[string]string{"membership": "leave"}},
	}}}}}})
	if err := mx.cryptoSync(ctx, "iris", leaveSync, "next-sync"); err != nil {
		t.Fatal(err)
	}
	remaining, err := mx.cryptoFor("iris").helper.Machine().CryptoStore.GetOutboundGroupSession(ctx, "!iris:chat.test")
	if err != nil || remaining != nil {
		t.Fatal("membership departure did not rotate outbound encryption keys")
	}
	entries, err := os.ReadDir(filepath.Join(c.Data, "secrets", "crypto", "iris"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, _ := entry.Info()
		if info.Mode().Perm()&0077 != 0 {
			t.Errorf("private crypto file permissions: %s", entry.Name())
		}
	}
}

func TestEncryptedAttachmentIntegrity(t *testing.T) {
	key := attachment.NewEncryptedFile()
	data := []byte("private file")
	key.EncryptInPlace(data)
	var info event.EncryptedFileInfo
	if err := json.Unmarshal(jsonBytes(key), &info); err != nil {
		t.Fatal(err)
	}
	info.URL = "mxc://chat.test/file"
	var ev Event
	if err := json.Unmarshal(jsonBytes(map[string]any{"content": map[string]any{"msgtype": "m.file", "body": "file.txt", "file": info}}), &ev); err != nil {
		t.Fatal(err)
	}
	a := incomingMedia(ev)
	if a.Error != "" || a.Source != string(info.URL) || a.File == nil {
		t.Fatal("encrypted attachment rejected")
	}
	if err := a.File.DecryptInPlace(data); err != nil || string(data) != "private file" {
		t.Fatal("file decryption failed", err)
	}
	var corrupt event.EncryptedFileInfo
	_ = json.Unmarshal(jsonBytes(info), &corrupt)
	if err := corrupt.DecryptInPlace([]byte("tampered")); err == nil {
		t.Fatal("corrupt ciphertext accepted")
	}
}

// Container state is delivered as an m.notice, which the default push rule
// .m.rule.suppress_notices matches, so a client shows it without notifying. A
// reply stays an m.text. The msgtype sits inside the encrypted payload, so this
// checks the decrypted content a client actually evaluates its push rules on.
func TestLifecycleNoticeIsSilentAndReplyIsNot(t *testing.T) {
	ctx := context.Background()
	c, store := testStore(t)
	f, server := newCryptoServer(t)
	bot := Credential{User: "@agent_iris:chat.test", Token: "@agent_iris:chat.test", Password: "test-only-password"}
	mx := &Matrix{Base: server.URL, c: c, crypto: map[string]*matrixCrypto{}, secrets: Secrets{Accounts: map[string]*Credential{"agent_iris": &bot}}}
	if err := mx.initCrypto(ctx, "iris", bot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mx.closeCrypto)
	if err := mx.ensureEncrypted(ctx, "iris", "!iris:chat.test"); err != nil {
		t.Fatal(err)
	}
	humanConfig := c
	humanConfig.Data = t.TempDir()
	human := &Matrix{Base: server.URL, c: humanConfig, crypto: map[string]*matrixCrypto{}}
	if err := human.initCrypto(ctx, "phone", Credential{User: "@admin:chat.test", Token: "@admin:chat.test", Password: "test-only-password"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(human.closeCrypto)
	phone := human.cryptoFor("phone")
	if _, err := phone.client.Members(ctx, "!iris:chat.test"); err != nil {
		t.Fatal(err)
	}
	if err := phone.client.StateStore.SetEncryptionEvent(ctx, "!iris:chat.test", &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}); err != nil {
		t.Fatal(err)
	}
	// Cross-sign the phone so the bot is willing to encrypt for it.
	if err := store.Update(func(st *State) error {
		addNoticeFull(st.Members["iris"], &Notice{ID: "silent", Room: "!iris:chat.test", Text: "Agent iris is starting…", Silent: true})
		addNoticeFull(st.Members["iris"], &Notice{ID: "loud", Room: "!iris:chat.test", Text: "the answer"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sent := 0
	for i := 0; i < 2; i++ {
		if err := mx.sendNext(ctx, store, "iris"); err != nil {
			t.Fatal(err)
		}
		sent++
	}
	f.mu.Lock()
	sends := append([]json.RawMessage(nil), f.sends...)
	deviceEvents := f.toDevice["@admin:chat.test"]
	f.mu.Unlock()
	if len(sends) != sent {
		t.Fatalf("expected %d sends, got %d", sent, len(sends))
	}
	// The phone learns the Megolm session from the bot's to-device key share.
	if err := human.cryptoSync(ctx, "phone", jsonBytes(map[string]any{"to_device": map[string]any{"events": deviceEvents}}), ""); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Agent iris is starting…": "m.notice", "the answer": "m.text"}
	for _, raw := range sends {
		var encrypted event.EncryptedEventContent
		if err := json.Unmarshal(raw, &encrypted); err != nil {
			t.Fatal("invalid reply")
		}
		room := jsonBytes(map[string]any{"type": "m.room.encrypted", "event_id": "$reply", "origin_server_ts": time.Now().UnixMilli(), "sender": bot.User, "content": &encrypted})
		got, err := human.decryptEvent(ctx, "phone", "!iris:chat.test", room)
		if err != nil {
			t.Fatal("phone could not decrypt reply", err)
		}
		kind, ok := want[got.Content.Body]
		if !ok {
			continue
		}
		if got.Content.MsgType != kind {
			t.Fatalf("message %q had msgtype %q, want %q", got.Content.Body, got.Content.MsgType, kind)
		}
		delete(want, got.Content.Body)
	}
	if len(want) != 0 {
		t.Fatalf("no message delivered for %v", want)
	}
}
