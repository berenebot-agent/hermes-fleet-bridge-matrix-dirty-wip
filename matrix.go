package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Credential struct{ Password, Token, User string }
type Secrets struct {
	Registration string
	Accounts     map[string]*Credential
	APIKeys      map[string]string
}
type Matrix struct {
	Base       string
	c          Config
	mu         sync.Mutex
	secrets    Secrets
	secretPath string
}

func newMatrix(c Config) (*Matrix, error) {
	m := &Matrix{Base: "http://" + c.MatrixListen, c: c, secretPath: filepath.Join(c.Data, "secrets", "accounts.json")}
	b, e := readPrivate(m.secretPath)
	if os.IsNotExist(e) {
		entries, re := os.ReadDir(filepath.Join(c.Data, "matrix"))
		if re != nil || len(entries) > 0 {
			return nil, errors.New("account secrets missing beside an existing Matrix database; restore a consistent backup")
		}
		m.secrets = Secrets{Registration: randomSecret(), Accounts: map[string]*Credential{}, APIKeys: map[string]string{}}
		e = m.saveSecrets()
	} else if e == nil {
		e = json.Unmarshal(b, &m.secrets)
	}
	if e != nil || !strongKey(m.secrets.Registration) || m.secrets.Accounts == nil || m.secrets.APIKeys == nil {
		return nil, errors.New("private credentials unavailable or corrupt; restore secrets with the database")
	}
	for name, a := range m.secrets.Accounts {
		if a == nil || a.User != "@"+name+":"+c.ServerName || len(a.Password) < 16 {
			return nil, errors.New("invalid private account record; restore a consistent backup")
		}
	}
	for _, key := range m.secrets.APIKeys {
		if !strongKey(key) {
			return nil, errors.New("invalid private API credential record")
		}
	}
	if e = os.Chmod(m.secretPath, 0600); e != nil {
		return nil, errors.New("cannot restrict account credential permissions")
	}
	if e = atomicFile(filepath.Join(c.Data, "secrets", "registration"), []byte(m.secrets.Registration)); e != nil {
		return nil, e
	}
	return m, nil
}
func (m *Matrix) saveSecrets() error { return atomicFile(m.secretPath, jsonBytes(m.secrets)) }
func (m *Matrix) account(ctx context.Context, name string, admin bool) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.secrets.Accounts[name]
	if a == nil {
		password := randomSecret()
		if admin {
			password = m.c.Password
		}
		a = &Credential{Password: password, User: "@" + name + ":" + m.c.ServerName}
		m.secrets.Accounts[name] = a
		if e := m.saveSecrets(); e != nil {
			return Credential{}, e
		}
	}
	if a.Token != "" {
		var who struct {
			User string `json:"user_id"`
		}
		e := api(ctx, "GET", m.Base, "/_matrix/client/v3/account/whoami", a.Token, nil, &who, nil)
		if e == nil && who.User == a.User {
			return *a, nil
		}
		if e != nil && !httpStatus(e, 401) {
			return Credential{}, e
		}
	}
	login := func(password string) error {
		var r struct {
			Token string `json:"access_token"`
			User  string `json:"user_id"`
		}
		e := api(ctx, "POST", m.Base, "/_matrix/client/v3/login", "", map[string]any{"type": "m.login.password", "identifier": map[string]string{"type": "m.id.user", "user": name}, "password": password, "device_id": "HERMES_FLEET_BRIDGE"}, &r, nil)
		if e == nil {
			if r.User != a.User || r.Token == "" {
				return errors.New("Matrix login identity mismatch")
			}
			a.Token = r.Token
			a.Password = password
		}
		return e
	}
	e := login(a.Password)
	if e != nil && !httpStatus(e, 403) && !httpStatus(e, 404) {
		return Credential{}, e
	}
	if e != nil && admin && a.Password != m.c.Password {
		e = login(m.c.Password)
	}
	if e != nil {
		var nonce struct {
			Nonce string `json:"nonce"`
		}
		if e = api(ctx, "GET", m.Base, "/_synapse/admin/v1/register", "", nil, &nonce, nil); e != nil {
			return Credential{}, e
		}
		role := "notadmin"
		if admin {
			role = "admin"
		}
		h := hmac.New(sha1.New, []byte(m.secrets.Registration))
		h.Write([]byte(strings.Join([]string{nonce.Nonce, name, a.Password, role}, "\x00")))
		e = api(ctx, "POST", m.Base, "/_synapse/admin/v1/register", "", map[string]any{"nonce": nonce.Nonce, "username": name, "password": a.Password, "admin": admin, "mac": hex.EncodeToString(h.Sum(nil))}, nil, nil)
		if e != nil {
			return Credential{}, errors.New("account provisioning failed; existing passwords are never reset")
		}
		if e = login(a.Password); e != nil {
			return Credential{}, errors.New("provisioned account login failed")
		}
	}
	if e = m.saveSecrets(); e != nil {
		return Credential{}, e
	}
	return *a, nil
}
func (m *Matrix) cred(name string) Credential {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.secrets.Accounts[name]
	if a == nil {
		return Credential{}
	}
	return *a
}
func (m *Matrix) key(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.secrets.APIKeys[id]
}
func (m *Matrix) rememberKey(id, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for other, k := range m.secrets.APIKeys {
		if other != id && k == key {
			return errors.New("Hermes API keys must be unique per agent")
		}
	}
	if old := m.secrets.APIKeys[id]; old != "" && old != key {
		return errors.New("Hermes API key changed; reconcile outstanding runs before credential rotation")
	}
	m.secrets.APIKeys[id] = key
	return m.saveSecrets()
}
func roomPath(room string) string { return "/_matrix/client/v3/rooms/" + url.PathEscape(room) }
func (m *Matrix) provision(ctx context.Context, s *Store, id string) error {
	bot, e := m.account(ctx, "agent_"+id, false)
	if e != nil {
		return e
	}
	admin := m.cred(m.c.Admin)
	member := s.View().Members[id]
	alias := "#hermes_" + id + ":" + m.c.ServerName
	var r struct {
		Room string `json:"room_id"`
	}
	if member.Room != "" {
		r.Room = member.Room
	} else {
		e = api(ctx, "GET", m.Base, "/_matrix/client/v3/directory/room/"+url.PathEscape(alias), bot.Token, nil, &r, nil)
		if httpStatus(e, 404) {
			body := map[string]any{"room_alias_name": "hermes_" + id, "name": id, "preset": "private_chat", "visibility": "private", "invite": []string{admin.User}, "creation_content": map[string]bool{"m.federate": false}, "initial_state": []any{map[string]any{"type": "org.hermes.fleet", "state_key": "", "content": map[string]string{"agent": id, "server": m.c.ServerName}}, map[string]any{"type": "m.room.history_visibility", "state_key": "", "content": map[string]string{"history_visibility": "joined"}}}}
			e = api(ctx, "POST", m.Base, "/_matrix/client/v3/createRoom", bot.Token, body, &r, nil)
			if e != nil {
				return errors.New("private room creation failed; alias is retained for safe reconciliation")
			}
		} else if e != nil {
			return e
		}
	}
	if r.Room == "" || (member.Room != "" && member.Room != r.Room) {
		return errors.New("Matrix room mapping differs from the saved room; refuse replacement")
	}
	var marker struct{ Agent, Server string }
	if e = api(ctx, "GET", m.Base, roomPath(r.Room)+"/state/org.hermes.fleet/", bot.Token, nil, &marker, nil); e != nil || marker.Agent != id || marker.Server != m.c.ServerName {
		return errors.New("existing room ownership marker does not match this fleet")
	}
	var membership struct{ Membership string }
	e = api(ctx, "GET", m.Base, roomPath(r.Room)+"/state/m.room.member/"+url.PathEscape(admin.User), bot.Token, nil, &membership, nil)
	if e != nil || membership.Membership != "join" {
		if membership.Membership != "invite" {
			if e = api(ctx, "POST", m.Base, roomPath(r.Room)+"/invite", bot.Token, map[string]string{"user_id": admin.User}, nil, nil); e != nil {
				return e
			}
		}
		if e = api(ctx, "POST", m.Base, "/_matrix/client/v3/join/"+url.PathEscape(r.Room), admin.Token, map[string]any{}, nil, nil); e != nil {
			return e
		}
	}
	if e = m.grantAdmin(ctx, r.Room, bot.Token, admin.User); e != nil {
		return e
	}
	return s.Update(func(st *State) error {
		member := st.Members[id]
		member.Room = r.Room
		if member.Rooms == nil {
			member.Rooms = map[string]*Room{}
		}
		if member.Rooms[r.Room] == nil {
			member.Rooms[r.Room] = &Room{}
		}
		return nil
	})
}

// grantAdmin gives the configured administrator real power in the agent's room. Tuwunel's
// private_chat default leaves the invited human at power level 0, which blocks renaming the
// room and filing it into a space. The bot is the room creator, so it can always repair this.
func (m *Matrix) grantAdmin(ctx context.Context, room, token, user string) error {
	var levels map[string]any
	if e := api(ctx, "GET", m.Base, roomPath(room)+"/state/m.room.power_levels/", token, nil, &levels, nil); e != nil {
		return e
	}
	users, _ := levels["users"].(map[string]any)
	if users == nil {
		users = map[string]any{}
	}
	if level, ok := users[user].(float64); ok && level >= 100 {
		return nil
	}
	users[user] = 100
	levels["users"] = users
	return api(ctx, "PUT", m.Base, roomPath(room)+"/state/m.room.power_levels/", token, levels, nil, nil)
}

type Event struct {
	ID       string `json:"event_id"`
	Sender   string `json:"sender"`
	Type     string `json:"type"`
	StateKey string `json:"state_key"`
	Content  struct {
		Body    string `json:"body"`
		MsgType string `json:"msgtype"`
		// Membership appears on m.room.member events (invite detection).
		Membership string `json:"membership"`
		// CreateType is the m.room.create content type, which marks a space.
		CreateType string `json:"type"`
		Mentions   struct {
			UserIDs []string `json:"user_ids"`
		} `json:"m.mentions"`
		Relation struct {
			Type  string `json:"rel_type"`
			ID    string `json:"event_id"`
			Key   string `json:"key"`
			Reply *struct {
				ID string `json:"event_id"`
			} `json:"m.in_reply_to"`
		} `json:"m.relates_to"`
	} `json:"content"`
}
type Timeline struct {
	Events  []Event `json:"events"`
	Limited bool    `json:"limited"`
	Prev    string  `json:"prev_batch"`
}

type syncRoomsResponse struct {
	Next  string `json:"next_batch"`
	Rooms struct {
		Join   map[string]joinedRoom  `json:"join"`
		Invite map[string]invitedRoom `json:"invite"`
		Leave  map[string]struct{}    `json:"leave"`
	} `json:"rooms"`
}
type joinedRoom struct {
	Timeline Timeline `json:"timeline"`
	State    struct {
		Events []Event `json:"events"`
	} `json:"state"`
}
type invitedRoom struct {
	InviteState struct {
		Events []Event `json:"events"`
	} `json:"invite_state"`
}

// acceptInvites joins the bot to every room an approved human added it to, spaces included,
// because room membership is the access boundary. An invite from another bot is never
// accepted: agents must not wake each other. A failed join is retried while the invite stands.
func (mx *Matrix) acceptInvites(ctx context.Context, m *Member, bot Credential, invites map[string]invitedRoom) {
	for room, invited := range invites {
		if _, known := m.Rooms[room]; !known && len(m.Rooms) >= maxRooms {
			continue
		}
		inviter := ""
		for _, ev := range invited.InviteState.Events {
			if ev.Type == "m.room.member" && ev.StateKey == bot.User && ev.Content.Membership == "invite" {
				inviter = ev.Sender
			}
		}
		if inviter == "" || botSender(inviter) {
			continue
		}
		_ = api(ctx, "POST", mx.Base, "/_matrix/client/v3/join/"+url.PathEscape(room), bot.Token, map[string]any{}, nil, nil)
	}
}

// reconcileInvites reads the current invitation list once per start. An incrementing sync only
// reports an invite relative to its cursor, so an invitation that arrived while the bridge was
// elsewhere would otherwise never be offered again; invitations are reconciled on every start.
func (mx *Matrix) reconcileInvites(ctx context.Context, s *Store, id string) error {
	m := s.View().Members[id]
	bot := mx.cred("agent_" + id)
	if bot.Token == "" {
		return errors.New("agent bot credentials unavailable")
	}
	filter := string(jsonBytes(map[string]any{"room": map[string]any{"timeline": map[string]int{"limit": 0}}, "presence": map[string]any{"types": []string{}}, "account_data": map[string]any{"types": []string{}}}))
	var response syncRoomsResponse
	if e := api(ctx, "GET", mx.Base, "/_matrix/client/v3/sync?timeout=0&filter="+url.QueryEscape(filter), bot.Token, nil, &response, nil); e != nil {
		return e
	}
	mx.acceptInvites(ctx, m, bot, response.Rooms.Invite)
	return nil
}

// roomScan is one room's processed sync window, applied to the store in a single update.
type roomScan struct {
	mention bool
	space   bool
	last    string
	turns   []*Turn
}

// localUser returns the localpart of a Matrix user ID, lowercased.
func localUser(user string) string {
	u := strings.TrimPrefix(user, "@")
	if i := strings.IndexByte(u, ':'); i >= 0 {
		u = u[:i]
	}
	return strings.ToLower(u)
}

// botSender reports whether a user belongs to the bridge's reserved agent namespace. Agent
// bots never wake each other; that would loop.
func botSender(user string) bool {
	la := localUser(user)
	return strings.HasPrefix(la, "agent_") || strings.HasPrefix(la, "agent.") || strings.HasPrefix(la, "agent-")
}

// mentioned reports whether a message addresses the agent: an explicit Matrix mention, or the
// bot's own user ID or localpart appearing in the raw body, which includes the quoted text a
// client puts in a reply. Localpeople are prefix-safe because bot localparts are long enough
// to stay distinguishable.
func mentioned(ev Event, bot Credential) bool {
	for _, u := range ev.Content.Mentions.UserIDs {
		if u == bot.User {
			return true
		}
	}
	la := localUser(bot.User)
	return strings.Contains(ev.Content.Body, bot.User) || strings.Contains(strings.ToLower(ev.Content.Body), la)
}

// stripMention removes a leading address to the agent so Hermes receives the instruction
// without the Matrix addressing. A message that is nothing but an address is left intact.
func stripMention(text string, bot Credential) string {
	la := localUser(bot.User)
	for _, prefix := range []string{bot.User, "@" + la, la} {
		if len(text) < len(prefix) || strings.ToLower(text[:len(prefix)]) != prefix {
			continue
		}
		rest := strings.TrimLeft(text[len(prefix):], " \t:,-")
		if strings.TrimSpace(rest) == "" {
			return text
		}
		return rest
	}
	return text
}

// roomIsSpace reports whether a room is a space container. Sync state answers it for newly
// joined rooms; otherwise the create event is fetched once and cached on the room.
func (mx *Matrix) roomIsSpace(ctx context.Context, token, room string, state []Event) (bool, error) {
	for _, ev := range state {
		if ev.Type == "m.room.create" {
			return ev.Content.CreateType == "m.space", nil
		}
	}
	var create struct {
		Type string `json:"type"`
	}
	if e := api(ctx, "GET", mx.Base, roomPath(room)+"/state/m.room.create/", token, nil, &create, nil); e != nil {
		return false, e
	}
	return create.Type == "m.space", nil
}

func (mx *Matrix) syncOnce(ctx context.Context, s *Store, id string) error {
	m := s.View().Members[id]
	bot := mx.cred("agent_" + id)
	if m.Room == "" || bot.Token == "" {
		return errors.New("agent room or bot credentials unavailable")
	}
	// One sync stream per agent covers its provisioned room plus every room the bot was invited
	// to. Timeline entries are capped per room so a full fleet stays inside the response limit,
	// and no room restriction is set because invites are only visible without one.
	filter := string(jsonBytes(map[string]any{"room": map[string]any{"timeline": map[string]any{"limit": 50, "types": []string{"m.room.message", "m.reaction"}}, "ephemeral": map[string]any{"types": []string{}}, "state": map[string]any{"lazy_load_members": true}}, "presence": map[string]any{"types": []string{}}, "account_data": map[string]any{"types": []string{}}}))
	q := url.Values{"timeout": {"25000"}, "filter": {filter}}
	if m.Since != "" {
		q.Set("since", m.Since)
	}
	var response syncRoomsResponse
	if e := api(ctx, "GET", mx.Base, "/_matrix/client/v3/sync?"+q.Encode(), bot.Token, nil, &response, nil); e != nil {
		return e
	}
	if response.Next == "" {
		return errors.New("Matrix sync did not return a cursor")
	}
	mx.acceptInvites(ctx, m, bot, response.Rooms.Invite)
	scans := map[string]*roomScan{}
	for room, joined := range response.Rooms.Join {
		scan := &roomScan{mention: room != m.Room}
		if known := m.Rooms[room]; known != nil {
			scan.mention, scan.space, scan.last = known.Mention, known.Space, known.LastEvent
		} else {
			space, e := mx.roomIsSpace(ctx, bot.Token, room, joined.State.Events)
			if e != nil {
				return e
			}
			scan.space = space
		}
		if room == m.Room {
			scan.mention = false
		}
		if scan.space {
			// A space is a container, not a conversation: join it, never dispatch from it.
			scans[room] = scan
			continue
		}
		events := joined.Timeline.Events
		if joined.Timeline.Limited && scan.last != "" {
			prefix, e := mx.backfill(ctx, bot.Token, room, scan.last, joined.Timeline.Prev)
			if e != nil {
				// The bridge's own room refuses to advance across missing history; a room the
				// operator created must not wedge every other room's sync, so it restarts from
				// this window instead.
				if room == m.Room {
					return e
				}
			} else {
				events = append(prefix, events...)
			}
		}
		for _, ev := range events {
			if ev.ID == "" {
				continue
			}
			scan.last = ev.ID
			if ev.Type == "m.reaction" && !botSender(ev.Sender) && ev.Content.Relation.Type == "m.annotation" {
				scan.turns = append(scan.turns, &Turn{ID: ev.ID, Room: room, Sender: ev.Sender, MenuID: ev.Content.Relation.ID, Choice: ev.Content.Relation.Key, Text: "!menu-choice", Kind: "command", Phase: "queued", Created: time.Now().UTC()})
				continue
			}
			if botSender(ev.Sender) || ev.Type != "m.room.message" || ev.Content.MsgType != "m.text" {
				continue
			}
			thread := ""
			if ev.Content.Relation.Type != "" {
				if ev.Content.Relation.Type != "m.thread" {
					continue
				}
				thread = ev.Content.Relation.ID
			}
			text := ev.Content.Body
			if ev.Content.Relation.Reply != nil && strings.HasPrefix(text, ">") {
				if pos := strings.Index(text, "\n\n"); pos >= 0 {
					text = text[pos+2:]
				}
			}
			t := &Turn{ID: ev.ID, Room: room, Thread: thread, Sender: ev.Sender, Text: stripMention(text, bot), Phase: "queued", Created: time.Now().UTC()}
			if strings.HasPrefix(strings.TrimSpace(t.Text), "!") {
				t.Kind = "command"
			}
			// In a room the operator created the bot behaves like any other member and answers
			// when addressed; in its own room a mention is never required.
			if t.Kind == "" && scan.mention && !mentioned(ev, bot) {
				continue
			}
			scan.turns = append(scan.turns, t)
		}
		scans[room] = scan
	}
	return s.Update(func(st *State) error {
		current := st.Members[id]
		if current.Rooms == nil {
			current.Rooms = map[string]*Room{}
		}
		for room, scan := range scans {
			tracked := current.Rooms[room]
			if tracked == nil {
				tracked = &Room{Mention: scan.mention, Space: scan.space}
				current.Rooms[room] = tracked
			}
			tracked.Space = scan.space
			if room == current.Room {
				tracked.Mention = false
			}
			if scan.last != "" {
				tracked.LastEvent = scan.last
			}
			for _, t := range scan.turns {
				if t.MenuID != "" {
					menuID := ""
					for key, menu := range current.Menus {
						if menu.EventID != "" && menu.EventID == t.MenuID && menu.Room == room && menu.Owner == t.Sender {
							menuID = key
							break
						}
					}
					if menuID == "" {
						continue
					}
					t.MenuID = menuID
				}
				if findTurn(current, t.ID) != nil {
					continue
				}
				if t.Thread != "" {
					t.Phase = "done"
					addNotice(current, t.ID+"-thread", room, "", "Thread prompts and commands are unsupported. Use this room’s main chat and !menu → Conversations → New / Resume. Existing thread history was preserved.")
				} else if !current.Enabled && t.Kind == "" {
					if room != m.Room {
						// A removed agent does not announce itself in operator-created rooms.
						continue
					}
					t.Phase = "done"
					addNotice(current, t.ID+"-disabled", room, t.Thread, "This agent has been removed from HERMES_DIRS; dispatch is disabled.")
				} else if len(t.Text) > 16<<10 || len(t.Root) > 16<<10 {
					t.Phase = "done"
					addNotice(current, t.ID+"-size", room, t.Thread, "Message or thread root exceeds the 16 KiB text limit; nothing was dispatched.")
				} else if pending(current) >= maxPending && t.Kind == "" {
					t.Phase = "done"
					addNotice(current, t.ID+"-busy", room, t.Thread, "Agent queue is full (100 turns); nothing was dispatched. Try again later.")
				}
				current.Turns = append(current.Turns, t)
			}
		}
		// A room the bot has left stops being served; a later re-invite starts a fresh
		// conversation rather than resuming a session nobody can see any more.
		for room := range response.Rooms.Leave {
			if room == m.Room {
				continue
			}
			delete(current.Rooms, room)
			delete(current.Sessions, room)
			for key, menu := range current.Menus {
				if menu.Room == room {
					delete(current.Menus, key)
				}
			}
			for _, t := range current.Turns {
				if t.Room == room && t.Kind == "" && (t.Phase == "queued" || t.Phase == "processing") {
					t.Phase = "done"
				}
			}
			kept := current.Outbox[:0]
			for _, n := range current.Outbox {
				if n.Room != room {
					kept = append(kept, n)
				}
			}
			current.Outbox = kept
		}
		current.Since = response.Next
		prune(current)
		return nil
	})
}
func (mx *Matrix) backfill(ctx context.Context, token, room, anchor, from string) ([]Event, error) {
	var reverse []Event
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		if from == "" || seen[from] {
			break
		}
		seen[from] = true
		var r struct {
			Chunk []Event `json:"chunk"`
			End   string  `json:"end"`
		}
		q := url.Values{"dir": {"b"}, "limit": {"100"}, "from": {from}}
		if e := api(ctx, "GET", mx.Base, roomPath(room)+"/messages?"+q.Encode(), token, nil, &r, nil); e != nil {
			return nil, e
		}
		found := false
		for _, ev := range r.Chunk {
			if ev.ID == anchor && anchor != "" {
				found = true
				break
			}
			reverse = append(reverse, ev)
		}
		if found || len(r.Chunk) == 0 || r.End == "" || r.End == from {
			if !found && anchor != "" {
				return nil, errors.New("Matrix history gap cannot be bridged; sync cursor retained")
			}
			for i, j := 0, len(reverse)-1; i < j; i, j = i+1, j-1 {
				reverse[i], reverse[j] = reverse[j], reverse[i]
			}
			return reverse, nil
		}
		from = r.End
	}
	return nil, errors.New("Matrix backfill exceeded safety limit; sync cursor retained")
}
func chunks(text string) []string {
	if text == "" {
		text = "(Hermes completed without a text reply.)"
	}
	var out []string
	for len(text) > 12000 {
		n := 12000
		for !utf8.RuneStart(text[n]) {
			n--
		}
		out = append(out, text[:n])
		text = text[n:]
	}
	return append(out, text)
}
func (mx *Matrix) sendNext(ctx context.Context, s *Store, id string) error {
	m := s.View().Members[id]
	if len(m.Outbox) == 0 {
		return nil
	}
	n := m.Outbox[0]
	if time.Now().Before(n.Next) || n.Attempts >= 8 {
		return nil
	}
	room := n.Room
	if room == "" {
		room = m.Room
	}
	if m.Rooms[room] == nil {
		// The bot is no longer in this room; drop the reply instead of blocking every later
		// reply behind an undeliverable head.
		return s.Update(func(st *State) error {
			a := st.Members[id]
			if len(a.Outbox) > 0 && a.Outbox[0].ID == n.ID {
				a.Outbox = a.Outbox[1:]
			}
			return nil
		})
	}
	parts := chunks(n.Text)
	bot := mx.cred("agent_" + id)
	if n.MenuID != "" && n.EventID != "" {
		return mx.seedMenu(ctx, s, id, n, bot)
	}
	content := map[string]any{"msgtype": "m.text", "body": parts[n.Sent]}
	if n.Thread != "" {
		content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": n.Thread, "is_falling_back": true, "m.in_reply_to": map[string]string{"event_id": n.Thread}}
	}
	txn := stableID(id, n.ID, string(rune(n.Sent)))
	var r struct {
		ID string `json:"event_id"`
	}
	e := api(ctx, "PUT", mx.Base, roomPath(room)+"/send/m.room.message/"+txn, bot.Token, content, &r, nil)
	if e == nil && r.ID == "" {
		e = errors.New("Matrix send returned no event ID")
	}
	return s.Update(func(st *State) error {
		a := st.Members[id]
		if len(a.Outbox) == 0 || a.Outbox[0].ID != n.ID {
			return nil
		}
		v := a.Outbox[0]
		if e != nil {
			v.Attempts++
			v.Next = time.Now().Add(time.Duration(1<<min(v.Attempts, 6)) * time.Second)
			return nil
		}
		v.Attempts = 0
		v.Next = time.Time{}
		if v.MenuID != "" {
			v.EventID = r.ID
			if menu := a.Menus[v.MenuID]; menu != nil {
				menu.EventID = r.ID
			}
			return nil
		}
		v.Sent++
		if v.Sent == len(parts) {
			a.Outbox = a.Outbox[1:]
		}
		return nil
	})
}

func (mx *Matrix) seedMenu(ctx context.Context, s *Store, id string, notice *Notice, bot Credential) error {
	for {
		m := s.View().Members[id]
		if len(m.Outbox) == 0 || m.Outbox[0].ID != notice.ID {
			return nil
		}
		n := m.Outbox[0]
		menu := m.Menus[n.MenuID]
		if menu == nil || !menuValid(m, menu) || n.Reaction >= len(menu.Options) {
			return s.Update(func(st *State) error {
				a := st.Members[id]
				if len(a.Outbox) > 0 && a.Outbox[0].ID == n.ID {
					a.Outbox = a.Outbox[1:]
				}
				return nil
			})
		}
		key := menu.Options[n.Reaction].Key
		content := map[string]any{"m.relates_to": map[string]string{"rel_type": "m.annotation", "event_id": n.EventID, "key": key}}
		var response struct {
			ID string `json:"event_id"`
		}
		err := api(ctx, "PUT", mx.Base, roomPath(n.Room)+"/send/m.reaction/"+stableID(id, n.ID, "reaction", key), bot.Token, content, &response, nil)
		if err == nil && response.ID == "" {
			err = errors.New("Matrix reaction send returned no event ID")
		}
		if updateErr := s.Update(func(st *State) error {
			a := st.Members[id]
			if len(a.Outbox) == 0 || a.Outbox[0].ID != n.ID {
				return nil
			}
			v := a.Outbox[0]
			if err != nil {
				v.Attempts++
				v.Next = time.Now().Add(time.Duration(1<<min(v.Attempts, 6)) * time.Second)
			} else {
				v.Reaction++
				v.Attempts = 0
				v.Next = time.Time{}
			}
			return nil
		}); updateErr != nil {
			return updateErr
		}
		if err != nil {
			return nil
		}
	}
}
func (mx *Matrix) typing(ctx context.Context, m *Member, room string, on bool) {
	bot := mx.cred("agent_" + m.ID)
	if room == "" {
		room = m.Room
	}
	_ = api(ctx, "PUT", mx.Base, roomPath(room)+"/typing/"+url.PathEscape(bot.User), bot.Token, map[string]any{"typing": on, "timeout": 25000}, nil, nil)
}
