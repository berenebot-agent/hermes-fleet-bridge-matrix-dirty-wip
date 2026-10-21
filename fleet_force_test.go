package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeFleetDocker serves the Docker API for a fleet test: the agent's own container plus any
// extra containers, and it records every stop so a test can assert what was shut down.
type fakeFleetDocker struct {
	containers []map[string]any
	stopped    []string
	// downLog is the fake CLI's command log. Once it records a `down`, this project's
	// containers are gone, exactly as Docker would have removed them.
	downLog string
	project string
}

func (f *fakeFleetDocker) sync() {
	if f.downLog == "" {
		return
	}
	b, err := os.ReadFile(f.downLog)
	if err != nil || !strings.Contains(string(b), " down ") {
		return
	}
	var kept []map[string]any
	for _, c := range f.containers {
		if labels, ok := c["Labels"].(map[string]string); ok && labels["com.docker.compose.project"] == f.project {
			continue
		}
		kept = append(kept, c)
	}
	f.containers = kept
}

func (f *fakeFleetDocker) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		f.sync()
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			replyJSON(w, f.containers)
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
			for _, c := range f.containers {
				if c["Id"] == id {
					replyJSON(w, c)
					return
				}
			}
			http.Error(w, "no such container", 404)
		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/stop"):
			id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/stop"), "/containers/"), "?t=60")
			f.stopped = append(f.stopped, id)
			replyJSON(w, map[string]any{})
		default:
			replyJSON(w, map[string]any{})
		}
	}
}

// fleetContainer builds a Docker container description with the given project and bind mounts.
func fleetContainer(id, project, dir string, mounts ...string) map[string]any {
	labels := map[string]string{"com.docker.compose.project": project, "com.docker.compose.service": "hermes", "com.docker.compose.project.working_dir": dir}
	var list []any
	for _, source := range mounts {
		list = append(list, map[string]any{"Type": "bind", "Source": source, "Destination": "/opt/data"})
	}
	return map[string]any{"Id": id, "Labels": labels, "Config": map[string]any{"Labels": labels}, "HostConfig": map[string]string{"NetworkMode": "host"}, "Mounts": list}
}

// fleetBrokenAgentFixture wires a fleet test whose agent API is unreachable, which is the state
// that used to make a delete wait forever with no explanation.
func fleetBrokenAgentFixture(t *testing.T) (*Engine, context.Context, *fakeFleetDocker, string) {
	t.Helper()
	e, ctx, _, log := fleetFixture(t)
	dir := e.s.View().Members["iris"].Dir
	fake := &fakeFleetDocker{containers: []map[string]any{fleetContainer("own", "iris", dir, filepath.Join(dir, "data"))}, downLog: log, project: "iris"}
	ds := httptest.NewServer(fake.handler(t))
	t.Cleanup(ds.Close)
	du, _ := url.Parse(ds.URL)
	e.docker.client = &http.Client{Transport: rewriteTransport{du, http.DefaultTransport}}
	// The agent's own API answers nothing, so its activity can never be proven idle.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", 503)
	}))
	t.Cleanup(broken.Close)
	bu, _ := url.Parse(broken.URL)
	port, _ := strconv.Atoi(bu.Port())
	_ = e.s.Update(func(st *State) error { st.Members["iris"].Port = port; return nil })
	return e, ctx, fake, log
}

// A delete blocked by an unreachable agent API must say so and offer the force, instead of
// waiting silently forever.
func TestFleetDeleteReportsUnreachableAPIAndOffersForce(t *testing.T) {
	e, ctx, _, _ := fleetBrokenAgentFixture(t)
	fleetTestCommand(t, e, ctx, "!fleet delete iris")
	cf := e.s.View().FleetConfirmation
	if cf == nil {
		t.Fatal("delete did not require confirmation")
	}
	fleetTestCommand(t, e, ctx, "!fleet confirm-delete iris "+cf.Code)
	op := e.s.View().FleetOperation
	if op == nil || op.Stage != "waiting" {
		t.Fatalf("delete did not reach the waiting stage: %+v", op)
	}
	e.fleetStep(ctx)
	op = e.s.View().FleetOperation
	if op == nil || op.Error == "" {
		t.Fatal("unreachable API did not hold the delete")
	}
	if !strings.Contains(op.Error, "unreachable") {
		t.Fatalf("held delete did not name the cause: %q", op.Error)
	}
	m := e.s.View().Members[fleetID]
	last := m.Outbox[len(m.Outbox)-1].Text
	if !strings.Contains(last, "!fleet force-delete iris") {
		t.Fatalf("held delete did not offer the force: %q", last)
	}
	if !strings.Contains(last, "cannot be drained") {
		t.Fatalf("force offer did not disclose the lost drain: %q", last)
	}
}

// The force delete must be confirmable and must then complete, erasing the agent.
func TestFleetForceDeleteCompletesWithoutDrain(t *testing.T) {
	e, ctx, _, _ := fleetBrokenAgentFixture(t)
	dir := e.s.View().Members["iris"].Dir
	fleetTestCommand(t, e, ctx, "!fleet force-delete iris")
	cf := e.s.View().FleetConfirmation
	if cf == nil || cf.Action != "force-delete" {
		t.Fatal("force delete did not require its own confirmation")
	}
	fleetTestCommand(t, e, ctx, "!fleet confirm-force-delete iris "+cf.Code)
	op := e.s.View().FleetOperation
	if op == nil || !op.Forced {
		t.Fatalf("force delete did not start as forced: %+v", op)
	}
	completeFleetTestOperation(t, e, ctx)
	if e.s.View().FleetOperation != nil {
		t.Fatal("forced delete did not complete")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("managed data not erased by the forced delete")
	}
	if !e.s.View().Members["iris"].Deleted {
		t.Fatal("forced delete did not retire the agent")
	}
}

// The force must also work when a normal delete is already stuck waiting on the same agent.
func TestFleetForceDeleteReplacesStuckDelete(t *testing.T) {
	e, ctx, _, _ := fleetBrokenAgentFixture(t)
	fleetTestCommand(t, e, ctx, "!fleet delete iris")
	cf := e.s.View().FleetConfirmation
	fleetTestCommand(t, e, ctx, "!fleet confirm-delete iris "+cf.Code)
	e.fleetStep(ctx)
	if e.s.View().FleetOperation == nil {
		t.Fatal("delete did not stay stuck")
	}
	fleetTestCommand(t, e, ctx, "!fleet force-delete iris")
	cf = e.s.View().FleetConfirmation
	if cf == nil {
		t.Fatal("force delete refused while a delete was stuck")
	}
	fleetTestCommand(t, e, ctx, "!fleet confirm-force-delete iris "+cf.Code)
	op := e.s.View().FleetOperation
	if op == nil || !op.Forced || op.Action != "delete" {
		t.Fatalf("stuck delete was not replaced by the force: %+v", op)
	}
	completeFleetTestOperation(t, e, ctx)
	if e.s.View().FleetOperation != nil {
		t.Fatal("forced delete did not complete")
	}
}

// A force delete must not disturb an operation on a different agent.
func TestFleetForceDeleteRefusesToDisturbAnotherOperation(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	fleetTestCommand(t, e, ctx, "!fleet pause iris")
	if e.s.View().FleetOperation == nil {
		t.Fatal("pause did not start an operation")
	}
	fleetTestCommand(t, e, ctx, "!fleet force-delete iris")
	cf := e.s.View().FleetConfirmation
	if cf == nil {
		t.Fatal("force delete refused while another operation was pending")
	}
	fleetTestCommand(t, e, ctx, "!fleet confirm-force-delete iris "+cf.Code)
	op := e.s.View().FleetOperation
	if op == nil || op.Action != "pause" {
		t.Fatalf("another agent's operation was disturbed: %+v", op)
	}
}

// A container outside the managed Compose project that mounts the agent's directory must stop
// the erase: erasing under a live mount leaves a process writing into a deleted path.
func TestFleetDeleteRefusesWhileUnmanagedContainerMountsTheDirectory(t *testing.T) {
	e, ctx, fake, _ := fleetBrokenAgentFixture(t)
	dir := e.s.View().Members["iris"].Dir
	// The stray project shares the same bind mount but carries a different project label.
	fake.containers = append(fake.containers, fleetContainer("stray", "other", dir, filepath.Join(dir, "data")))
	fleetTestCommand(t, e, ctx, "!fleet force-delete iris")
	cf := e.s.View().FleetConfirmation
	fleetTestCommand(t, e, ctx, "!fleet confirm-force-delete iris "+cf.Code)
	for i := 0; i < 8 && e.s.View().FleetOperation != nil; i++ {
		e.fleetStep(ctx)
		if op := e.s.View().FleetOperation; op != nil && op.Error != "" {
			break
		}
	}
	op := e.s.View().FleetOperation
	if op == nil || op.Error == "" {
		t.Fatal("erase proceeded while a container still mounted the directory")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("managed directory was erased under a live mount")
	}
	if len(fake.stopped) == 0 {
		t.Fatal("the container holding the mount was not stopped")
	}
}

// A busy agent is a normal wait, not a fault: the delete must keep waiting without holding
// the operation with an error, and without offering the force.
func TestFleetDeleteWaitsForBusyAgentWithoutOfferingForce(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	dir := e.s.View().Members["iris"].Dir
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replyJSON(w, map[string]any{"status": "ok", "gateway_busy": true})
	}))
	t.Cleanup(busy.Close)
	bu, _ := url.Parse(busy.URL)
	port, _ := strconv.Atoi(bu.Port())
	fake := &fakeFleetDocker{containers: []map[string]any{fleetContainer("own", "iris", dir, filepath.Join(dir, "data"))}}
	ds := httptest.NewServer(fake.handler(t))
	t.Cleanup(ds.Close)
	du, _ := url.Parse(ds.URL)
	e.docker.client = &http.Client{Transport: rewriteTransport{du, http.DefaultTransport}}
	_ = e.s.Update(func(st *State) error { st.Members["iris"].Port = port; return nil })
	fleetTestCommand(t, e, ctx, "!fleet delete iris")
	cf := e.s.View().FleetConfirmation
	fleetTestCommand(t, e, ctx, "!fleet confirm-delete iris "+cf.Code)
	e.fleetStep(ctx)
	op := e.s.View().FleetOperation
	if op == nil || op.Stage != "waiting" {
		t.Fatalf("busy agent did not keep the delete waiting: %+v", op)
	}
	if op.Error != "" {
		t.Fatalf("busy agent held the delete as a fault: %q", op.Error)
	}
	m := e.s.View().Members[fleetID]
	for _, n := range m.Outbox {
		if strings.Contains(n.Text, "force-delete") {
			t.Fatalf("force was offered for a merely busy agent: %q", n.Text)
		}
	}
}

// A normal (unforced) delete must not stop an unrelated container: it never reaches the
// mount check because it cannot pass the drain stage first.
func TestFleetNormalDeleteDoesNotStopUnrelatedContainers(t *testing.T) {
	e, ctx, fake, _ := fleetBrokenAgentFixture(t)
	dir := e.s.View().Members["iris"].Dir
	fake.containers = append(fake.containers, fleetContainer("stray", "other", dir, filepath.Join(dir, "data")))
	fleetTestCommand(t, e, ctx, "!fleet delete iris")
	cf := e.s.View().FleetConfirmation
	fleetTestCommand(t, e, ctx, "!fleet confirm-delete iris "+cf.Code)
	e.fleetStep(ctx)
	if len(fake.stopped) != 0 {
		t.Fatalf("a normal delete stopped unrelated containers: %v", fake.stopped)
	}
}
