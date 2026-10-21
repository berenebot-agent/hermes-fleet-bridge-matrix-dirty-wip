package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAPIUnavailableMessage(t *testing.T) {
	for _, kind := range []string{"running", "stopped", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			m := &Member{Project: "iris", Service: "hermes", Dir: "/iris", Port: 8642}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "unknown" {
					http.Error(w, "private-docker-error", 500)
				} else if kind == "stopped" {
					replyJSON(w, []any{})
				} else {
					labels := map[string]string{"com.docker.compose.project": "iris", "com.docker.compose.service": "hermes", "com.docker.compose.project.working_dir": "/iris"}
					if r.URL.Path == "/containers/json" {
						replyJSON(w, []any{map[string]any{"Id": "container", "Labels": labels}})
					} else {
						replyJSON(w, map[string]any{"Config": map[string]any{"Labels": labels}, "HostConfig": map[string]string{"NetworkMode": "host"}})
					}
				}
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			e := &Engine{docker: &Docker{client: &http.Client{Transport: rewriteTransport{u, http.DefaultTransport}}}}
			got := e.apiUnavailableMessage(context.Background(), m)
			want := map[string]string{"running": "container is running, but its Hermes API did not become ready", "stopped": "container is not running", "unknown": "Cannot reach a ready, durably idempotent Hermes API"}[kind]
			if !strings.Contains(got, want) || !strings.Contains(got, "No automatic replay") || strings.Contains(got, "private-docker-error") {
				t.Fatalf("unsafe or incorrect notice: %s", got)
			}
		})
	}
}

func TestStartupFailureQueuesSpecificNotice(t *testing.T) {
	e, s := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "private-provider-error", 503)
	})
	e.c.Start = 20 * time.Millisecond
	if err := s.Update(func(st *State) error {
		st.Members["iris"].Turns = []*Turn{{ID: "startup-failure", Text: "hello", Phase: "queued"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.work(context.Background(), "iris", "startup-failure")
	m := s.View().Members["iris"]
	if findTurn(m, "startup-failure").Phase != "held" {
		t.Fatal("startup failure must remain held without automatic replay")
	}
	for _, notice := range m.Outbox {
		if notice.ID == "startup-failure-failure" {
			if !strings.Contains(notice.Text, "container is running") || strings.Contains(notice.Text, "private-provider-error") {
				t.Fatal("notice must explain the symptom without raw errors")
			}
			return
		}
	}
	t.Fatal("held turn did not queue a durable failure notice")
}
