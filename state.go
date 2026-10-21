package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const stateVersion = 7
const maxPending = 100

// maxRooms bounds how many rooms one agent serves. Invites beyond it stay pending rather than
// letting an operator grow the per-agent sync response without limit.
const maxRooms = 100

type State struct {
	Version                      int
	ServerName, PublicURL, Admin string
	Members                      map[string]*Member
	FleetOperation               *FleetOperation
	FleetConfirmation            *FleetConfirmation
	FleetPrompt                  *FleetPrompt
	FleetDir, TemplateDir        string
}

// FleetPrompt is a short-lived request for a destination name. It is separate from
// FleetConfirmation: a confirmation expects an echoed code, a prompt expects the
// operator's next plain message. Only one interaction is pending at a time.
type FleetPrompt struct {
	Action, Source, Owner string
	Expires               time.Time
}
type Member struct {
	ID, Dir, Home, Project, Service, Network       string
	Files                                          []string
	Port                                           int
	Enabled                                        bool
	Name                                           string
	Managed, Controller, Paused, Draining, Deleted bool
	SyncFailed                                     bool
	Room, Since                                    string
	Rooms                                          map[string]*Room
	Sessions                                       map[string]map[string]*Session
	InitialSessions                                map[string]*Session
	Menus                                          map[string]*Menu
	DefaultModel, DefaultProvider                  string
	Turns                                          []*Turn
	Outbox                                         []*Notice
	Encrypted                                      []EncryptedPending
	LastActivity                                   time.Time
}

// Room is one Matrix room this agent serves: its provisioned room, plus any room an approved
// user has invited the bot to. Cursors are per room because backfill anchors are per room,
// while the sync token is a single per-member stream.
type Room struct {
	LastEvent string
	Mention   bool
	Space     bool
	Selected  string
	Revision  uint64
	// HideTools suppresses the agent's tool-call summaries in this room.
	HideTools bool
	// WidgetToken authorises the room's widget panel. The widget URL carries it, so it is
	// readable only by room members.
	WidgetToken string
}
type Session struct {
	ID, Model, Provider string
	Seeded              bool
	Title               string
	LastUsed            time.Time
}
type Turn struct {
	ID, Room, Thread, Text, Root, Kind, Phase, RunID string
	Sender, MenuID, Choice                           string
	Selection                                        *MenuOption
	MenuRevision                                     uint64
	Request                                          json.RawMessage
	Submitted                                        time.Time
	Retention                                        int
	Stop                                             bool
	Approval                                         string
	Attempts                                         int
	Created                                          time.Time
	Media                                            *Attachment
	Completed                                        time.Time
	MediaCleaned                                     bool
	// ToolSeq is the next run-event sequence this turn expects. It resumes the
	// Hermes event stream after a controller restart without repeating a line.
	ToolSeq int
	// ToolLines counts the tool summaries already queued for this turn, so the
	// per-turn cap holds across a reconnect.
	ToolLines int
}
type Notice struct {
	ID, Room, Thread, Text string
	Plain                  bool
	// Silent marks a container-lifecycle notice. It is delivered as an m.notice, which the
	// default push rule .m.rule.suppress_notices matches, so a client shows it in the
	// timeline without notifying. Only container state is silent: replies, menus and
	// approval requests stay ordinary messages and still notify.
	Silent     bool
	MenuID     string
	EventID    string
	Reaction   int
	Sent       int
	Attempts   int
	Next       time.Time
	Media      *Attachment
	TurnID     string
	Ciphertext json.RawMessage `json:",omitempty"`
}

// Ciphertext is retained before acknowledging sync, including while keys are delayed.
type EncryptedPending struct {
	Room           string
	Raw            json.RawMessage
	Received       time.Time
	KeyRequests    int
	NextKeyRequest time.Time
}

// stateV1 and memberV1 are the frozen state version 1 on-disk shapes. Never edit them: they
// exist only so an existing controller state file can be upgraded in place.
type stateV1 struct {
	Version                      int
	ServerName, PublicURL, Admin string
	Members                      map[string]memberV1
}
type memberV1 struct {
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

// loadState decodes a persisted state file, upgrading the version 1 layout by wrapping the
// single room and session map under the member's provisioned room.
func loadState(b []byte, out *State) (bool, error) {
	var head struct {
		Version int
	}
	if json.Unmarshal(b, &head) != nil {
		return false, errors.New("state version unreadable")
	}
	switch head.Version {
	case stateVersion:
		return false, json.Unmarshal(b, out)
	case 2, 3, 4, 5, 6:
		if err := json.Unmarshal(b, out); err != nil {
			return false, err
		}
		out.Version = stateVersion
		return true, nil
	case 1:
		var old stateV1
		if json.Unmarshal(b, &old) != nil {
			return false, errors.New("version 1 state unreadable")
		}
		st := State{Version: stateVersion, ServerName: old.ServerName, PublicURL: old.PublicURL, Admin: old.Admin, Members: map[string]*Member{}}
		for id, m := range old.Members {
			n := &Member{ID: m.ID, Dir: m.Dir, Home: m.Home, Project: m.Project, Service: m.Service, Network: m.Network, Files: m.Files, Port: m.Port, Enabled: m.Enabled, SyncFailed: m.SyncFailed, Room: m.Room, Since: m.Since, Turns: m.Turns, Outbox: m.Outbox, LastActivity: m.LastActivity, Rooms: map[string]*Room{}, Sessions: map[string]map[string]*Session{}}
			if m.Room != "" {
				n.Rooms[m.Room] = &Room{LastEvent: m.LastEvent}
				n.Sessions[m.Room] = m.Sessions
			}
			for _, t := range n.Turns {
				t.Room = m.Room
			}
			for _, o := range n.Outbox {
				o.Room = m.Room
			}
			st.Members[id] = n
		}
		*out = st
		return true, nil
	default:
		return false, errors.New("unknown state version")
	}
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
	migrated := false
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
			migrated, e = loadState(b, &s.state)
		}
	}
	if e != nil {
		f.Close()
		return nil, errors.New("controller state unreadable or corrupt; restore a consistent backup, do not delete state")
	}
	if s.state.Version != stateVersion || s.state.ServerName != c.ServerName || s.state.Admin != c.Admin || s.state.Members == nil {
		f.Close()
		return nil, errors.New("state identity/version differs from configuration; restore the original MATRIX_SERVER_NAME and ADMIN_USERNAME (only PUBLIC_URL and ADMIN_PASSWORD may change)")
	}
	if s.state.FleetDir != "" && (s.state.FleetDir != c.FleetDir || s.state.TemplateDir != c.TemplateDir) {
		f.Close()
		return nil, errors.New("managed storage paths differ from persisted ownership; restore the original fleet configuration")
	}
	if op := s.state.FleetOperation; op != nil {
		if !validFleetOperation(op, s.state) {
			f.Close()
			return nil, errors.New("invalid fleet operation state; refusing unsafe recovery")
		}
	}
	for id, m := range s.state.Members {
		if m == nil || m.ID != id || !localpart.MatchString(id) {
			f.Close()
			return nil, errors.New("invalid member state; restore a consistent backup")
		}
		if m.Rooms == nil {
			m.Rooms = map[string]*Room{}
		}
		if m.Sessions == nil {
			m.Sessions = map[string]map[string]*Session{}
		}
		if m.Room != "" && m.Rooms[m.Room] == nil {
			m.Rooms[m.Room] = &Room{}
		}
		if m.Rooms[m.Room] != nil {
			m.Rooms[m.Room].Mention = false
		}
		for room, r := range m.Rooms {
			if r == nil || (r.Selected != "" && (m.Sessions[room] == nil || m.Sessions[room][r.Selected] == nil || (r.Selected != "room" && !strings.HasPrefix(r.Selected, "chat:")))) {
				f.Close()
				return nil, errors.New("invalid room conversation state; restore a consistent backup")
			}
		}
		for _, sessions := range m.Sessions {
			for _, session := range sessions {
				if session == nil || session.ID == "" {
					f.Close()
					return nil, errors.New("invalid session state; restore a consistent backup")
				}
			}
		}
		for key, session := range m.InitialSessions {
			if session == nil || session.ID == "" || (key != "room" && !strings.HasPrefix(key, "chat:")) {
				f.Close()
				return nil, errors.New("invalid initial copied conversation state")
			}
		}
		for _, menu := range m.Menus {
			if menu == nil || len(menu.Options) > 8 || len(menu.Options) == 0 {
				f.Close()
				return nil, errors.New("invalid menu state; restore a consistent backup")
			}
		}
		for _, t := range m.Turns {
			if t == nil || t.ID == "" {
				f.Close()
				return nil, errors.New("invalid turn state; restore a consistent backup")
			}
			if t.Room == "" {
				t.Room = m.Room
			}
			if t.Media != nil && !validAttachment(t.Media, false) {
				f.Close()
				return nil, errors.New("invalid inbound media state; restore a consistent backup")
			}
			switch t.Phase {
			case "queued", "preparing", "submitting", "running", "held", "processing", "done":
			default:
				f.Close()
				return nil, errors.New("unknown turn phase; refuse unsafe recovery")
			}
		}
		if len(m.Encrypted) > 1000 || len(jsonBytes(m.Encrypted)) > 16<<20 {
			f.Close()
			return nil, errors.New("encrypted inbox exceeds safety limit")
		}
		for _, p := range m.Encrypted {
			var ev Event
			if p.Room == "" || p.Received.IsZero() || p.KeyRequests < 0 || p.KeyRequests > 8 || json.Unmarshal(p.Raw, &ev) != nil || ev.Type != "m.room.encrypted" || ev.ID == "" {
				f.Close()
				return nil, errors.New("invalid encrypted inbox; restore a consistent backup")
			}
		}
		for _, n := range m.Outbox {
			if n == nil || n.ID == "" || n.Sent < 0 || n.Sent >= len(chunks(n.Text)) || n.Reaction < 0 || n.Reaction > 8 {
				f.Close()
				return nil, errors.New("invalid reply outbox; restore a consistent backup")
			}
			if n.Room == "" {
				n.Room = m.Room
			}
			if n.Media != nil && (n.TurnID == "" || !validAttachment(n.Media, true)) {
				f.Close()
				return nil, errors.New("invalid attachment outbox; restore a consistent backup")
			}
			if len(n.Ciphertext) > 0 {
				var wire struct {
					Algorithm string `json:"algorithm"`
				}
				if len(n.Ciphertext) > 1<<20 || json.Unmarshal(n.Ciphertext, &wire) != nil || wire.Algorithm != "m.megolm.v1.aes-sha2" {
					f.Close()
					return nil, errors.New("invalid encrypted reply state")
				}
			}
		}
	}
	if s.state.PublicURL != c.PublicURL {
		s.state.PublicURL = c.PublicURL
		migrated = true
	}
	if migrated {
		if e = atomicFile(s.path, jsonBytes(s.state)); e != nil {
			f.Close()
			return nil, e
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
				if t.Completed.IsZero() {
					t.Completed = time.Now().UTC()
				}
				t.Text = ""
				t.Root = ""
				t.Request = nil
				if t.Media != nil {
					t.Media.Transcript = ""
				}
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
func addNotice(m *Member, id, room, thread, text string) {
	addNoticeFull(m, &Notice{ID: id, Room: room, Thread: thread, Text: text})
}

// addNoticeFull queues a notice that carries more than plain text, such as a plain-body echo
// of a voice transcript. The ID still decides whether it is already queued.
func addNoticeFull(m *Member, n *Notice) {
	for _, e := range m.Outbox {
		if e.ID == n.ID {
			return
		}
	}
	m.Outbox = append(m.Outbox, n)
}

// noticeIndex locates a queued reply by ID. The outbox is shared by every room an agent serves,
// so entries are addressed by ID rather than by position.
func noticeIndex(m *Member, id string) int {
	for i, n := range m.Outbox {
		if n.ID == id {
			return i
		}
	}
	return -1
}

// removeNotice drops one queued reply, keeping the rest of the queue intact.
func removeNotice(m *Member, id string) {
	if i := noticeIndex(m, id); i >= 0 {
		m.Outbox = append(m.Outbox[:i], m.Outbox[i+1:]...)
	}
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

// threadKey names one conversation inside a room; an empty thread is the room's default one.
func threadKey(thread string) string {
	if thread == "" {
		return "room"
	}
	return thread
}

// sessionKey identifies a conversation for the in-memory scheduler, which is scoped per agent
// and must therefore separate rooms as well as threads.
func sessionKey(room, thread string) string { return room + "\x00" + threadKey(thread) }

func getSession(m *Member, room, thread string) *Session {
	if m.Sessions[room] == nil {
		m.Sessions[room] = map[string]*Session{}
	}
	k := threadKey(thread)
	if thread == "" && m.Rooms[room] != nil && m.Rooms[room].Selected != "" {
		k = m.Rooms[room].Selected
	}
	if m.Sessions[room][k] == nil {
		m.Sessions[room][k] = &Session{ID: "matrix_" + stableID(room, k), Model: m.DefaultModel, Provider: m.DefaultProvider}
	}
	return m.Sessions[room][k]
}

// conversationLabel names a conversation for operator-facing output, keeping the provisioned
// room's wording unchanged.
func conversationLabel(m *Member, room, thread string) string {
	if room == m.Room || room == "" {
		return threadKey(thread)
	}
	return room + "/" + threadKey(thread)
}

// turnRoom is the room a turn belongs to. An empty room means the agent's provisioned room,
// which keeps turns written before rooms were tracked on the same conversation.
func turnRoom(m *Member, t *Turn) string {
	if t.Room == "" {
		return m.Room
	}
	return t.Room
}
func prune(m *Member) {
	if len(m.Turns) < 10000 {
		return
	}
	out := m.Turns[:0]
	for _, t := range m.Turns {
		if t.Phase != "done" || time.Since(t.Completed) < 7*24*time.Hour || !t.MediaCleaned {
			out = append(out, t)
		}
	}
	m.Turns = out
}
func stateError(op string) error { return fmt.Errorf("%s failed; private details suppressed", op) }
