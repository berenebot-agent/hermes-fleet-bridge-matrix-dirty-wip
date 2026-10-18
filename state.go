package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const stateVersion = 1
const maxPending = 100

type State struct {
	Version                      int
	ServerName, PublicURL, Admin string
	Members                      map[string]*Member
}
type Member struct {
	ID, Dir, Home, Project, Service, Network string
	Files                                    []string
	Port                                     int
	Enabled                                  bool
	SyncFailed                               bool
	Room, Since, LastEvent                   string
	Sessions                                 map[string]*Session
	Turns                                    []*Turn
	Outbox                                   []*Notice
	LastActivity                             time.Time
}
type Session struct {
	ID, Model, Provider string
	Seeded              bool
}
type Turn struct {
	ID, Thread, Text, Root, Kind, Phase, RunID string
	Request                                    json.RawMessage
	Submitted                                  time.Time
	Retention                                  int
	Stop                                       bool
	Approval                                   string
	Attempts                                   int
	Created                                    time.Time
}
type Notice struct {
	ID, Thread, Text string
	Sent             int
	Attempts         int
	Next             time.Time
}
type Store struct {
	mu     sync.Mutex
	state  State
	path   string
	lock   *os.File
	failed error
	Fatal  chan error
}

func secureDir(p string) error {
	if e := os.MkdirAll(p, 0700); e != nil {
		return errors.New("cannot create private data directory")
	}
	i, e := os.Lstat(p)
	if e != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
		return errors.New("private data path must be a real directory")
	}
	if e = os.Chmod(p, 0700); e != nil {
		return errors.New("cannot restrict private data permissions")
	}
	return nil
}
func atomicFile(p string, b []byte) error {
	f, e := os.CreateTemp(filepath.Dir(p), ".replace-*")
	if e != nil {
		return errors.New("cannot create state replacement")
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(name, p)
	}
	if e == nil {
		var d *os.File
		d, e = os.Open(filepath.Dir(p))
		if e == nil {
			e = d.Sync()
			d.Close()
		}
	}
	if e != nil {
		return errors.New("durable private file replacement failed; check disk space and permissions")
	}
	return nil
}
func readPrivate(p string) ([]byte, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("private state must be a regular file, not a symlink")
	}
	if os.Chmod(p, 0600) != nil {
		return nil, errors.New("cannot restrict private file permissions")
	}
	return os.ReadFile(p)
}
func openStore(c Config) (*Store, error) {
	for _, p := range []string{c.Data, filepath.Join(c.Data, "controller"), filepath.Join(c.Data, "secrets"), filepath.Join(c.Data, "matrix")} {
		if e := secureDir(p); e != nil {
			return nil, e
		}
	}
	f, e := os.OpenFile(filepath.Join(c.Data, "controller", "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, errors.New("cannot open controller lock")
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil, errors.New("another controller owns this data directory")
	}
	s := &Store{path: filepath.Join(c.Data, "controller", "state.json"), lock: f, Fatal: make(chan error, 1)}
	b, e := readPrivate(s.path)
	if os.IsNotExist(e) {
		entries, re := os.ReadDir(filepath.Join(c.Data, "matrix"))
		if re != nil || len(entries) > 0 {
			f.Close()
			return nil, errors.New("controller state missing beside an existing Matrix database; restore a consistent backup")
		}
		s.state = State{Version: stateVersion, ServerName: c.ServerName, PublicURL: c.PublicURL, Admin: c.Admin, Members: map[string]*Member{}}
		e = atomicFile(s.path, jsonBytes(s.state))
	} else if e == nil {
		if len(b) > 64<<20 {
			e = errors.New("state exceeds 64 MiB limit")
		} else {
			e = json.Unmarshal(b, &s.state)
		}
	}
	if e != nil {
		f.Close()
		return nil, errors.New("controller state unreadable or corrupt; restore a consistent backup, do not delete state")
	}
	if s.state.Version != stateVersion || s.state.ServerName != c.ServerName || s.state.PublicURL != c.PublicURL || s.state.Admin != c.Admin || s.state.Members == nil {
		f.Close()
		return nil, errors.New("state identity/version differs from configuration; refuse reset or rename")
	}
	for id, m := range s.state.Members {
		if m == nil || m.ID != id || !localpart.MatchString(id) || m.Sessions == nil {
			f.Close()
			return nil, errors.New("invalid member state; restore a consistent backup")
		}
		for _, t := range m.Turns {
			if t == nil || t.ID == "" {
				f.Close()
				return nil, errors.New("invalid turn state; restore a consistent backup")
			}
			switch t.Phase {
			case "queued", "preparing", "submitting", "running", "held", "processing", "done":
			default:
				f.Close()
				return nil, errors.New("unknown turn phase; refuse unsafe recovery")
			}
		}
		for _, n := range m.Outbox {
			if n == nil || n.ID == "" || n.Sent < 0 || n.Sent >= len(chunks(n.Text)) {
				f.Close()
				return nil, errors.New("invalid reply outbox; restore a consistent backup")
			}
		}
	}
	return s, nil
}
func (s *Store) Close() { syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN); s.lock.Close() }
func (s *Store) View() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v State
	json.Unmarshal(jsonBytes(s.state), &v)
	return v
}
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return s.failed
	}
	if e := fn(&s.state); e != nil {
		return e
	}
	for _, m := range s.state.Members {
		for _, t := range m.Turns {
			if t.Phase == "done" {
				t.Text = ""
				t.Root = ""
				t.Request = nil
			}
		}
	}
	b := jsonBytes(s.state)
	var e error
	if len(b) > 64<<20 {
		e = errors.New("controller state reached 64 MiB safety limit")
	} else {
		e = atomicFile(s.path, b)
	}
	if e != nil {
		s.failed = e
		select {
		case s.Fatal <- e:
		default:
		}
		return e
	}
	return nil
}
func addNotice(m *Member, id, thread, text string) {
	for _, n := range m.Outbox {
		if n.ID == id {
			return
		}
	}
	m.Outbox = append(m.Outbox, &Notice{ID: id, Thread: thread, Text: text})
}
func findTurn(m *Member, id string) *Turn {
	for _, t := range m.Turns {
		if t.ID == id {
			return t
		}
	}
	return nil
}
func activePhase(p string) bool {
	return p == "preparing" || p == "submitting" || p == "running" || p == "held"
}
func pending(m *Member) int {
	n := 0
	for _, t := range m.Turns {
		if t.Phase == "queued" || activePhase(t.Phase) {
			n++
		}
	}
	return n
}
func sessionKey(thread string) string {
	if thread == "" {
		return "room"
	}
	return thread
}
func getSession(m *Member, thread string) *Session {
	k := sessionKey(thread)
	if m.Sessions[k] == nil {
		m.Sessions[k] = &Session{ID: "matrix_" + stableID(m.Room, k)}
	}
	return m.Sessions[k]
}
func prune(m *Member) {
	if len(m.Turns) < 10000 {
		return
	}
	out := m.Turns[:0]
	for _, t := range m.Turns {
		if t.Phase != "done" || time.Since(t.Created) < 7*24*time.Hour {
			out = append(out, t)
		}
	}
	m.Turns = out
}
func stateError(op string) error { return fmt.Errorf("%s failed; private details suppressed", op) }
