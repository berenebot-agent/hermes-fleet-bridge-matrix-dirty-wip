package main

import (
	"context"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Hermes names every session itself: an instant title cut from the opening message, then a
// small-model upgrade to a few words. The bridge does not pay for a second naming call —
// it reads the name Hermes already chose and puts it where the user can see it.
//
// The visible name of an operator-created room is "<agent> — <session name>", so several
// sessions with the same agent stay distinguishable in the room list. The agent's own
// provisioned room keeps the plain agent name: it is the home of the agent, and only
// !fleet rename may change it.

// titleCheckDelay is the shortest gap between two lookups of a session's name.
const titleCheckDelay = 10 * time.Second

// titleCheckWindow is how long the bridge keeps looking for a name after a turn. Hermes
// writes the instant title with the first message and upgrades it to the model title from
// a background thread once the turn settles, so the name can arrive after the reply.
const titleCheckWindow = 60 * time.Second

// titleUpgradeWindow is the shorter window kept once a name was found, so the model
// upgrade can still replace the instant title.
const titleUpgradeWindow = 20 * time.Second

// maxSessionTitle bounds the name the bridge will use. Hermes caps its own titles at 100
// characters; a room name also carries the agent's name, and a room name is display text.
const maxSessionTitle = 60

// selectedSession returns the persisted session a room currently uses, or nil. It mirrors
// getSession's selection without creating a session, so a read never invents state.
func selectedSession(m *Member, room string) *Session {
	if m == nil || room == "" || m.Sessions[room] == nil {
		return nil
	}
	key := threadKey("")
	if m.Rooms[room] != nil && m.Rooms[room].Selected != "" {
		key = m.Rooms[room].Selected
	}
	return m.Sessions[room][key]
}

// sessionName reads the name Hermes gave a session. It returns "" when the session has no
// name yet, which is normal: Hermes names a session from its first user message, and a
// session the bridge only opened has none.
func (e *Engine) sessionName(ctx context.Context, endpoint, id, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	var page struct {
		Session struct {
			Title string `json:"title"`
		} `json:"session"`
		Title string `json:"title"`
	}
	if api(ctx, "GET", endpoint, "/api/sessions/"+url.PathEscape(sessionID), e.mx.key(id), nil, &page, nil) != nil {
		return ""
	}
	title := page.Session.Title
	if title == "" {
		title = page.Title
	}
	return cleanSessionTitle(title)
}

// cleanSessionTitle makes a model-generated name safe to show as a room name: one line, no
// control characters, bounded length, and no surrounding punctuation. A name is untrusted
// text like any other message.
func cleanSessionTitle(title string) string {
	title = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, title)), " ")
	title = strings.Trim(title, " -–—:;,.·•\u2022")
	if len(title) > maxSessionTitle {
		title = title[:maxSessionTitle]
		for len(title) > 0 && !utf8.ValidString(title) {
			title = title[:len(title)-1]
		}
		title = strings.TrimRight(title, " -–—:;,.·•\u2022")
	}
	return title
}

// roomNameFor builds the visible name of a room: the agent's name, and the session's name
// when the session has one. The provisioned room keeps the plain agent name.
func roomNameFor(agent, title string, dedicated bool) string {
	agent = strings.TrimSpace(agent)
	if dedicated || title == "" {
		return agent
	}
	return agent + " — " + title
}

// refreshRoomName brings one room's visible name up to date with the name Hermes gave the
// session the room is currently using. It is deliberately forgiving: a room that cannot be
// renamed, or a Hermes API that cannot be reached, leaves the visible name alone. The name
// is display text, not state the bridge depends on.
//
// Two rules keep this from fighting anything else:
//
//   - The provisioned room is left alone: only !fleet rename owns that name.
//   - A name the bridge did not write is never overwritten. Room.RoomName records
//     only what the bridge itself wrote, so a name the operator chose survives.
func (e *Engine) refreshRoomName(ctx context.Context, id, room, endpoint string) {
	m := e.s.View().Members[id]
	if m == nil || room == "" {
		return
	}
	tracked := m.Rooms[room]
	if tracked == nil || tracked.Space {
		return
	}
	session := selectedSession(m, room)
	if session == nil || session.ID == "" {
		return
	}
	name := e.sessionName(ctx, endpoint, id, session.ID)
	want := ""
	// A room whose visible name the bridge did not write belongs to the operator,
	// and is never overwritten. The room's current name is only read when a write
	// is actually needed.
	owned := tracked.RoomName != ""
	_ = e.s.Update(func(st *State) error {
		member := st.Members[id]
		if member == nil {
			return nil
		}
		room2 := member.Rooms[room]
		if room2 == nil {
			return nil
		}
		s := getSession(member, room, "")
		s.TitleChecked = time.Now().UTC()
		if name != "" {
			s.SessionName = name
			// A name arrived: shorten the window so the model title can still
			// replace the instant one, but the lookup stops on its own.
			if s.TitleUntil.After(time.Now().UTC().Add(titleUpgradeWindow)) {
				s.TitleUntil = time.Now().UTC().Add(titleUpgradeWindow)
			}
		}
		if room == member.Room {
			// Only !fleet rename owns the provisioned room's name.
			return nil
		}
		if s.SessionName == "" {
			// The conversation has no name yet. A name the bridge wrote for the
			// previous conversation is reset to the agent's own name, so a
			// switched conversation does not keep showing the old one.
			if room2.RoomName == "" {
				return nil
			}
			want = roomNameFor(agentName(member), "", false)
		} else {
			want = roomNameFor(agentName(member), s.SessionName, false)
		}
		if want == room2.RoomName {
			want = ""
			return nil
		}
		room2.RoomName = want
		return nil
	})
	if want == "" {
		return
	}
	if !owned && e.mx.roomName(ctx, id, room) != "" {
		// The operator named this room; leave their name alone.
		_ = e.s.Update(func(st *State) error {
			if member := st.Members[id]; member != nil {
				if room2 := member.Rooms[room]; room2 != nil {
					room2.RoomName = ""
				}
			}
			return nil
		})
		return
	}
	_ = e.mx.setRoomName(ctx, id, room, want)
}

// renameSharedRooms moves the shared rooms of one agent to a new agent name. A shared
// room's visible name is "<agent> — <session name>", so the prefix moves with the
// rename. The session names are already known, so this needs no Hermes call.
func (e *Engine) renameSharedRooms(ctx context.Context, id, agent string) {
	m := e.s.View().Members[id]
	if m == nil {
		return
	}
	for room := range m.Rooms {
		if room == m.Room {
			continue
		}
		s := selectedSession(m, room)
		if s == nil || s.SessionName == "" {
			// A room the bridge never named keeps the name it has.
			continue
		}
		want := roomNameFor(agent, s.SessionName, false)
		if want == m.Rooms[room].RoomName {
			continue
		}
		if e.mx.setRoomName(ctx, id, room, want) != nil {
			continue
		}
		_ = e.s.Update(func(st *State) error {
			if member := st.Members[id]; member != nil {
				if tracked := member.Rooms[room]; tracked != nil {
					tracked.RoomName = want
				}
			}
			return nil
		})
	}
}

// titleDue reports whether a room's session name is worth looking up again. Hermes writes
// the model title from a background thread after the turn settles, so the bridge looks
// again for a while after the first name arrived, then stops asking.
//
// The window is opened once, when a turn is submitted, and shortened once a name is
// found: the model title can still replace the instant title, but the lookup stops on
// its own instead of running forever.
func titleDue(m *Member, room string) bool {
	if m == nil {
		return false
	}
	tracked := m.Rooms[room]
	if tracked == nil || tracked.Space {
		return false
	}
	s := selectedSession(m, room)
	if s == nil || s.ID == "" {
		return false
	}
	if s.TitleUntil.IsZero() {
		// Never looked up: one lookup now, to learn a name the session already has.
		return true
	}
	if time.Now().After(s.TitleUntil) {
		return false
	}
	return time.Since(s.TitleChecked) >= titleCheckDelay
}

// ensureSessionName keeps a room's visible name in step with the session Hermes named.
// It runs on the agent's own tick, so a name that arrives late is still applied. The
// lookup is stamped before the request, so an agent that cannot be reached is not polled
// on every tick.
func (e *Engine) ensureSessionName(ctx context.Context, id, room string) {
	m := e.s.View().Members[id]
	if m == nil || !titleDue(m, room) {
		return
	}
	// The lookup is stamped first, so an agent that cannot be reached is not
	// polled on every tick.
	_ = e.s.Update(func(st *State) error {
		if member := st.Members[id]; member != nil {
			if s := selectedSession(member, room); s != nil {
				s.TitleChecked = time.Now().UTC()
			}
		}
		return nil
	})
	endpoint, err := e.docker.endpoint(ctx, m)
	if err != nil {
		return
	}
	e.refreshRoomName(ctx, id, room, endpoint)
}
