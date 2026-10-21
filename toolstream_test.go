package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// toolStreamServer is a fake Hermes API that also serves the run event stream.
// The first status poll reports a running turn, the later ones a finished one,
// so the stream has delivered its frames before the turn completes.
type toolStreamServer struct {
	mu     sync.Mutex
	polls  int
	frames []string
	// delay holds the stream's frames back, so the run can finish while a tool
	// call is still in flight — the race the ordering barrier exists for.
	delay time.Duration
	// completeFirst reports the run as finished on the first status poll.
	completeFirst bool
}

func (s *toolStreamServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/capabilities":
		fakeCapabilities(w)
	case r.URL.Path == "/health/detailed":
		fakeHealth(w)
	case r.URL.Path == "/v1/runs" && r.Method == "POST":
		replyJSON(w, map[string]string{"run_id": "run-1"})
	case strings.HasSuffix(r.URL.Path, "/events"):
		w.Header().Set("Content-Type", "text/event-stream")
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		for _, f := range s.frames {
			w.Write([]byte(f))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	case strings.HasPrefix(r.URL.Path, "/v1/runs/"):
		s.mu.Lock()
		s.polls++
		polls := s.polls
		s.mu.Unlock()
		if polls == 1 && !s.completeFirst {
			replyJSON(w, map[string]string{"status": "running"})
			return
		}
		replyJSON(w, map[string]string{"status": "completed", "output": "the answer"})
	default:
		replyJSON(w, map[string]string{})
	}
}

func toolFrame(seq int, tool, preview string) string {
	return "data: {\"event\":\"tool.started\",\"tool\":\"" + tool + "\",\"preview\":\"" + preview + "\",\"seq\":" + strconv.Itoa(seq) + "}\n\n"
}

func TestToolSummariesPrecedeTheAnswer(t *testing.T) {
	stream := &toolStreamServer{frames: []string{
		toolFrame(1, "terminal", "ls -la /data"),
		"data: {\"event\":\"message.delta\",\"delta\":\"private working text\",\"seq\":2}\n\n",
		toolFrame(3, "read_file", "notes.md"),
		"data: {\"event\":\"run.completed\",\"output\":\"the answer\",\"seq\":4}\n\n",
	}}
	engine, s := fakeEngine(t, stream.ServeHTTP)
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "t1", Room: "!iris:chat.test", Phase: "queued"}}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go engine.work(ctx, "iris", "t1")
	await(t, func() bool { return len(s.View().Members["iris"].Outbox) >= 3 })
	out := s.View().Members["iris"].Outbox
	if out[0].Text != "🔧 terminal: ls -la /data" || out[1].Text != "🔧 read_file: notes.md" {
		t.Fatalf("unexpected tool summaries: %q then %q", out[0].Text, out[1].Text)
	}
	if out[2].Text != "the answer" {
		t.Fatalf("answer did not follow the summaries: %q", out[2].Text)
	}
	for _, n := range out {
		if strings.Contains(n.Text, "private working text") {
			t.Fatal("output delta leaked into a summary")
		}
	}
}

func TestToolSummaryInFlightWhenTheRunFinishes(t *testing.T) {
	// The run reports itself finished while a tool call is still travelling on the
	// event stream. The summary must still reach the room before the answer.
	stream := &toolStreamServer{
		frames:        []string{toolFrame(1, "terminal", "ls -la /data")},
		delay:         300 * time.Millisecond,
		completeFirst: true,
	}
	engine, s := fakeEngine(t, stream.ServeHTTP)
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "t1", Room: "!iris:chat.test", Phase: "queued"}}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go engine.work(ctx, "iris", "t1")
	await(t, func() bool { return len(s.View().Members["iris"].Outbox) >= 2 })
	out := s.View().Members["iris"].Outbox
	if out[0].Text != "🔧 terminal: ls -la /data" || out[1].Text != "the answer" {
		t.Fatalf("a summary in flight did not precede the answer: %q then %q", out[0].Text, out[1].Text)
	}
}

func TestToolSummariesHiddenPerRoom(t *testing.T) {
	stream := &toolStreamServer{frames: []string{toolFrame(1, "terminal", "ls -la /data")}}
	engine, s := fakeEngine(t, stream.ServeHTTP)
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "t1", Room: "!iris:chat.test", Phase: "queued"}}
		st.Members["iris"].Rooms["!iris:chat.test"].HideTools = true
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go engine.work(ctx, "iris", "t1")
	await(t, func() bool { return len(s.View().Members["iris"].Outbox) >= 1 })
	out := s.View().Members["iris"].Outbox
	if len(out) != 1 || out[0].Text != "the answer" {
		t.Fatalf("hidden room still showed summaries: %+v", out)
	}
}

func TestToolSummariesStopAtThePerTurnCap(t *testing.T) {
	var frames []string
	for i := 1; i <= toolSummaryLimit+5; i++ {
		frames = append(frames, toolFrame(i, "terminal", "call "+strconv.Itoa(i)))
	}
	stream := &toolStreamServer{frames: frames}
	engine, s := fakeEngine(t, stream.ServeHTTP)
	s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "t1", Room: "!iris:chat.test", Phase: "queued"}}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go engine.work(ctx, "iris", "t1")
	await(t, func() bool {
		for _, n := range s.View().Members["iris"].Outbox {
			if strings.Contains(n.Text, "further tool calls are not shown") {
				return true
			}
		}
		return false
	})
	lines := 0
	for _, n := range s.View().Members["iris"].Outbox {
		if strings.HasPrefix(n.Text, "🔧 ") {
			lines++
		}
	}
	if lines != toolSummaryLimit {
		t.Fatalf("expected %d summaries, got %d", toolSummaryLimit, lines)
	}
}

func TestToolsCommandTogglesTheRoom(t *testing.T) {
	engine, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) { replyJSON(w, map[string]string{}) })
	menuCommandTest(t, engine, "c1", "!iris:chat.test", "!tools off")
	if !s.View().Members["iris"].Rooms["!iris:chat.test"].HideTools {
		t.Fatal("!tools off did not hide the summaries")
	}
	menuCommandTest(t, engine, "c2", "!iris:chat.test", "!tools on")
	if s.View().Members["iris"].Rooms["!iris:chat.test"].HideTools {
		t.Fatal("!tools on did not show the summaries")
	}
	menuCommandTest(t, engine, "c3", "!iris:chat.test", "!tools")
	out := s.View().Members["iris"].Outbox
	if !strings.Contains(out[len(out)-1].Text, "Usage: !tools on|off.") {
		t.Fatalf("!tools without a value did not report its usage: %q", out[len(out)-1].Text)
	}
}
