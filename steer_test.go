package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const steerRoom = "!iris:chat.test"

// steerFixture wires an agent with one running turn and one queued follow-up in the
// same conversation.
func steerFixture(t *testing.T, e *Engine, s *Store, follow *Turn) {
	t.Helper()
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Turns = []*Turn{{ID: "run", Room: steerRoom, Sender: menuOwner, Text: "hello", Phase: "running", RunID: "run-1"}, follow}
		return nil
	})
}

func TestFollowUpIsSteeredIntoTheRunningTurn(t *testing.T) {
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
	steerFixture(t, e, s, &Turn{ID: "follow", Room: steerRoom, Sender: menuOwner, Text: "also check the logs", Phase: "queued"})
	e.steer(context.Background(), "iris", "follow")
	mu.Lock()
	got := steered
	mu.Unlock()
	if !strings.Contains(got, "/v1/runs/run-1/steer") || !strings.Contains(got, "also check the logs") {
		t.Fatalf("follow-up was not steered into the running turn: %q", got)
	}
	m := s.View().Members["iris"]
	if phase := findTurn(m, "follow").Phase; phase != "done" {
		t.Fatalf("steered follow-up is not finished: %q", phase)
	}
	if reply := lastReply(t, e); !strings.Contains(reply, "Steered into the running turn") {
		t.Fatalf("steer acknowledgement missing: %q", reply)
	}
}

func TestSteeredAttachmentCarriesTheStagedPathOnly(t *testing.T) {
	var mu sync.Mutex
	var steered string
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/steer") {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			steered = body["input"]
			mu.Unlock()
			replyJSON(w, map[string]any{"accepted": true})
			return
		}
		t.Errorf("unexpected API call %s", r.URL.Path)
	})
	home := t.TempDir()
	data := []byte("image-bytes")
	hash := sha256.Sum256(data)
	follow := &Turn{ID: "pic", Room: steerRoom, Sender: menuOwner, Text: "look at this", Phase: "queued", Media: &Attachment{Kind: "m.image", Name: "shot.png", MIME: "image/png", Ready: true, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}}
	os.MkdirAll(filepath.Join(home, "matrix-media", "incoming", stableID(follow.ID)), 0700)
	os.WriteFile(filepath.Join(home, "matrix-media", "incoming", stableID(follow.ID), "file"), data, 0600)
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Home = home
		m.Turns = []*Turn{{ID: "run", Room: steerRoom, Sender: menuOwner, Text: "hello", Phase: "running", RunID: "run-1"}, follow}
		return nil
	})
	e.steer(context.Background(), "iris", "pic")
	mu.Lock()
	got := steered
	mu.Unlock()
	if !strings.Contains(got, "/opt/data/matrix-media/incoming/"+stableID("pic")+"/file") {
		t.Fatalf("steered attachment does not carry the staged path: %q", got)
	}
	if !strings.Contains(got, "vision tool") {
		t.Fatalf("steered image does not tell the agent how to read it: %q", got)
	}
	// The reply belongs to the running turn, so the steered text must not point the
	// agent at another outgoing directory.
	if strings.Contains(got, "MATRIX_FILE") || strings.Contains(got, "matrix-media/outgoing") {
		t.Fatalf("steered attachment carries the file-transport marker: %q", got)
	}
	if findTurn(s.View().Members["iris"], "pic").Phase != "done" {
		t.Fatal("steered attachment was not finished")
	}
}

func TestRefusedSteerFallsBackToTheQueue(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/steer") {
			w.WriteHeader(http.StatusConflict)
			replyJSON(w, map[string]any{"errcode": "run_not_accepting_steer"})
			return
		}
		t.Errorf("unexpected API call %s", r.URL.Path)
	})
	steerFixture(t, e, s, &Turn{ID: "follow", Room: steerRoom, Sender: menuOwner, Text: "also check the logs", Phase: "queued"})
	e.steer(context.Background(), "iris", "follow")
	m := s.View().Members["iris"]
	if phase := findTurn(m, "follow").Phase; phase != "queued" {
		t.Fatalf("a refused steer did not return the message to the queue: %q", phase)
	}
	if reply := lastReply(t, e); !strings.Contains(reply, "queued") {
		t.Fatalf("fallback notice missing: %q", reply)
	}
}

func TestFollowUpInAnotherRoomIsNotSteered(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a follow-up in another room reached Hermes: %s", r.URL.Path)
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Turns = []*Turn{{ID: "run", Room: steerRoom, Sender: menuOwner, Text: "hello", Phase: "running", RunID: "run-1"}, {ID: "other", Room: "!other:chat.test", Sender: menuOwner, Text: "elsewhere", Phase: "queued"}}
		return nil
	})
	e.steer(context.Background(), "iris", "other")
	if phase := findTurn(s.View().Members["iris"], "other").Phase; phase != "queued" {
		t.Fatalf("a follow-up in another room was steered: %q", phase)
	}
}

func TestHeldTurnIsNeverSteeredInto(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("an uncertain run was steered into: %s", r.URL.Path)
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Turns = []*Turn{{ID: "run", Room: steerRoom, Sender: menuOwner, Text: "hello", Phase: "held", RunID: "run-1"}, {ID: "follow", Room: steerRoom, Sender: menuOwner, Text: "also check the logs", Phase: "queued"}}
		return nil
	})
	e.steer(context.Background(), "iris", "follow")
	if phase := findTurn(s.View().Members["iris"], "follow").Phase; phase != "queued" {
		t.Fatalf("a follow-up was steered into an uncertain run: %q", phase)
	}
}

func TestRestartHoldsAnUncertainSteer(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("an uncertain steer reached Hermes: %s", r.URL.Path)
	})
	s.Update(func(st *State) error {
		m := st.Members["iris"]
		m.Turns = []*Turn{{ID: "follow", Room: steerRoom, Sender: menuOwner, Text: "also check the logs", Phase: "steering", SteerRunID: "run-1"}}
		return nil
	})
	e.tick(context.Background(), "iris")
	m := s.View().Members["iris"]
	if phase := findTurn(m, "follow").Phase; phase != "held" {
		t.Fatalf("an uncertain steer was not held: %q", phase)
	}
	if reply := lastReply(t, e); !strings.Contains(reply, "not known whether the agent received it") {
		t.Fatalf("held steer notice missing: %q", reply)
	}
}

func TestSchedulerSteersAFollowUpWhileTheConversationIsBusy(t *testing.T) {
	var steers atomic.Int32
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/steer"):
			steers.Add(1)
			replyJSON(w, map[string]any{"accepted": true})
		case r.URL.Path == "/v1/capabilities":
			fakeCapabilities(w)
		case r.URL.Path == "/health/detailed":
			fakeHealth(w)
		case r.URL.Path == "/v1/runs/run-1":
			replyJSON(w, map[string]any{"run_id": "run-1", "status": "running"})
		case r.URL.Path == "/v1/runs/run-1/events":
			w.WriteHeader(404)
		default:
			t.Errorf("unexpected API call %s", r.URL.Path)
		}
	})
	steerFixture(t, e, s, &Turn{ID: "follow", Room: steerRoom, Sender: menuOwner, Text: "also check the logs", Phase: "queued"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.tick(ctx, "iris")
	await(t, func() bool { return steers.Load() == 1 })
	await(t, func() bool { return findTurn(s.View().Members["iris"], "follow").Phase == "done" })
}

func TestSteerIsOfferedOnlyOnce(t *testing.T) {
	var steers atomic.Int32
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/steer"):
			steers.Add(1)
			w.WriteHeader(http.StatusConflict)
			replyJSON(w, map[string]any{"errcode": "run_not_accepting_steer"})
		case r.URL.Path == "/v1/capabilities":
			fakeCapabilities(w)
		case r.URL.Path == "/health/detailed":
			fakeHealth(w)
		case r.URL.Path == "/v1/runs/run-1":
			replyJSON(w, map[string]any{"run_id": "run-1", "status": "running"})
		case r.URL.Path == "/v1/runs/run-1/events":
			w.WriteHeader(404)
		default:
			t.Errorf("unexpected API call %s", r.URL.Path)
		}
	})
	steerFixture(t, e, s, &Turn{ID: "follow", Room: steerRoom, Sender: menuOwner, Text: "also check the logs", Phase: "queued"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.tick(ctx, "iris")
	await(t, func() bool { return steers.Load() == 1 })
	await(t, func() bool { return findTurn(s.View().Members["iris"], "follow").Phase == "queued" })
	e.tick(ctx, "iris")
	if steers.Load() != 1 {
		t.Fatalf("the refused steer was offered again: %d attempts", steers.Load())
	}
}
