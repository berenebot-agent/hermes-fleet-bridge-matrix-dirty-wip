package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func dockerTestServer(t *testing.T, handler http.HandlerFunc) *Docker {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	return &Docker{client: &http.Client{Transport: rewriteTransport{u, http.DefaultTransport}}}
}

// Docker answers 304 when a container is already in the requested state. A stop
// whose goal is "not running" has succeeded, so it must not fail the operation.
func TestPostAcceptsAlreadyInState(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		d := dockerTestServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
		if err := d.post(context.Background(), "/containers/x/stop?t=60"); err != nil {
			t.Fatalf("status %d rejected: %v", status, err)
		}
	}
	d := dockerTestServer(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	if err := d.post(context.Background(), "/containers/x/stop?t=60"); err == nil {
		t.Fatal("a failed stop must report an error")
	}
}

// An exited container still reports its bind mounts but can no longer write into
// them. Counting it as a holder blocked deletion once the operation had stopped
// the project's own containers.
func TestMountHoldersIgnoreExitedContainers(t *testing.T) {
	dir := "/members/iris"
	running := map[string]any{"Id": "running", "State": map[string]any{"Running": true}, "Mounts": []any{map[string]string{"Type": "bind", "Source": dir + "/data"}}}
	exited := map[string]any{"Id": "exited", "State": map[string]any{"Running": false}, "Mounts": []any{map[string]string{"Type": "bind", "Source": dir + "/data"}}}
	unrelated := map[string]any{"Id": "other", "State": map[string]any{"Running": true}, "Mounts": []any{map[string]string{"Type": "bind", "Source": "/members/other/data"}}}
	// An unreported state cannot be shown to be safe, so it still counts.
	unknown := map[string]any{"Id": "unknown", "Mounts": []any{map[string]string{"Type": "bind", "Source": dir + "/data"}}}
	d := dockerTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/json":
			replyJSON(w, []any{map[string]string{"Id": "running"}, map[string]string{"Id": "exited"}, map[string]string{"Id": "other"}, map[string]string{"Id": "unknown"}})
		case "/containers/running/json":
			replyJSON(w, running)
		case "/containers/exited/json":
			replyJSON(w, exited)
		case "/containers/unknown/json":
			replyJSON(w, unknown)
		default:
			replyJSON(w, unrelated)
		}
	})
	holders, err := d.mountHolders(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, id := range holders {
		got[id] = true
	}
	if len(holders) != 2 || !got["running"] || !got["unknown"] {
		t.Fatalf("holders = %v, want the running and unreported containers", holders)
	}
}
