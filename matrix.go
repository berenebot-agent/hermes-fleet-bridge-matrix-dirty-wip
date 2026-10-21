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
	Retired      map[string]bool
}
type Matrix struct {
	Base       string
	c          Config
	mu         sync.Mutex
	secrets    Secrets
	secretPath string
	crypto     map[string]*matrixCrypto
}

func newMatrix(c Config) (*Matrix, error) {
	m := &Matrix{Base: "http://" + c.MatrixListen, c: c, secretPath: filepath.Join(c.Data, "secrets", "accounts.json"), crypto: map[string]*matrixCrypto{}}
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
	if m.secrets.Retired[name] {
		return Credential{}, errors.New("Matrix account is retired; refuse login or recreation")
	}
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
	verify := func() error {
		var who struct {
			User string `json:"user_id"`
		}
		if e := api(ctx, "GET", m.Base, "/_matrix/client/v3/account/whoami", a.Token, nil, &who, nil); e != nil {
			return e
		}
		if who.User != a.User {
			return errors.New("Matrix authenticated identity mismatch")
		}
		return nil
	}
	if admin && a.Password != m.c.Password {
		previous := *a
		saved := false
		defer func() {
			if !saved {
				*a = previous
			}
		}()
		// Try the desired login first: the remote change may have survived a lost response/restart.
		if e := login(m.c.Password); e != nil {
			if !httpStatus(e, 401) && !httpStatus(e, 403) && !httpStatus(e, 404) {
				return Credential{}, errors.New("administrator password reconciliation unavailable; private credentials retained")
			}
			var proof error
			if a.Token != "" {
				proof = verify()
			}
			if a.Token == "" || httpStatus(proof, 401) {
				proof = login(previous.Password)
				if proof == nil {
					proof = verify()
				}
			}
			if proof != nil {
				return Credential{}, errors.New("cannot authenticate existing administrator for password change; private credentials retained")
			}
			// Verification, not the reset response, decides success when a response is lost.
			_ = api(ctx, "POST", m.Base, "/_synapse/admin/v1/reset_password/"+url.PathEscape(a.User), a.Token, map[string]any{"new_password": m.c.Password, "logout_devices": true}, nil, nil)
			if e = login(m.c.Password); e != nil {
				return Credential{}, errors.New("administrator password change could not be verified; private credentials retained, retry with the same ADMIN_PASSWORD")
			}
		}
		if verify() != nil {
			return Credential{}, errors.New("new administrator login could not be verified; retry with the same ADMIN_PASSWORD")
		}
		if e := m.saveSecrets(); e != nil {
			return Credential{}, e
		}
		saved = true
		return *a, nil
	}
	if a.Token != "" {
		e := verify()
		if e == nil {
			return *a, nil
		}
		if !httpStatus(e, 401) {
			return Credential{}, e
		}
	}
	e := login(a.Password)
	if e != nil && !httpStatus(e, 403) && !httpStatus(e, 404) {
		return Credential{}, e
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
			return Credential{}, errors.New("account provisioning failed; registration never overwrites an existing account")
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
			body["name"] = agentName(member)
			if m.crypto != nil {
				body["initial_state"] = append(body["initial_state"].([]any), map[string]any{"type": "m.room.encryption", "state_key": "", "content": map[string]string{"algorithm": "m.megolm.v1.aes-sha2"}})
			}
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
	if e = m.nameBot(ctx, bot, agentName(member)); e != nil {
		return e
	}
	if m.crypto != nil {
		if e = m.initCrypto(ctx, id, bot); e != nil {
			return e
		}
		if e = m.ensureEncrypted(ctx, id, r.Room); e != nil {
			return e
		}
	}
	return s.Update(func(st *State) error {
		member := st.Members[id]
		firstFleetRoom := member.Controller && member.Room == ""
		member.Room = r.Room
		if member.Rooms == nil {
			member.Rooms = map[string]*Room{}
		}
		if member.Rooms[r.Room] == nil {
			member.Rooms[r.Room] = &Room{}
		}
		if member.InitialSessions != nil {
			member.Sessions[r.Room] = member.InitialSessions
			member.InitialSessions = nil
		}
		if firstFleetRoom {
			fleetMenu(st, &Turn{ID: "fleet-welcome", Sender: admin.User}, "home", "", m.c.TemplateDir)
		}
		return nil
	})
}

// nameBot gives the bot the agent's own name, so Element's member list and mention picker show
// the agent instead of the internal agent_ localpart. Reconciliation skips an unchanged name.
func (m *Matrix) nameBot(ctx context.Context, bot Credential, display string) error {
	path := "/_matrix/client/v3/profile/" + url.PathEscape(bot.User) + "/displayname"
	var profile struct {
		DisplayName string `json:"displayname"`
	}
	if e := api(ctx, "GET", m.Base, path, bot.Token, nil, &profile, nil); e != nil {
		return e
	}
	if profile.DisplayName == display {
		return nil
	}
	return api(ctx, "PUT", m.Base, path, bot.Token, map[string]string{"displayname": display}, nil, nil)
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

// purgeRoom erases a deleted agent's own room. Tuwunel evicts every local member
// (so the room leaves the operator's room list), removes the room's local aliases
// and purges its stored events, so the agent's Matrix history is erased rather than
// retained. Purging an already-purged room succeeds, so a retried delete is safe.
func (mx *Matrix) purgeRoom(ctx context.Context, room string) error {
	admin := mx.cred(mx.c.Admin)
	return api(ctx, "DELETE", mx.Base, "/_synapse/admin/v1/rooms/"+url.PathEscape(room), admin.Token, map[string]any{"purge": true, "block": true}, nil, nil)
}

type Event struct {
	Raw       json.RawMessage `json:"-"`
	Decrypted bool            `json:"-"`
	ID        string          `json:"event_id"`
	Sender    string          `json:"sender"`
	Type      string          `json:"type"`
	StateKey  string          `json:"state_key"`
	Content   struct {
		Body     string          `json:"body"`
		MsgType  string          `json:"msgtype"`
		URL      string          `json:"url"`
		Filename string          `json:"filename"`
		File     json.RawMessage `json:"file"`
		Voice    json.RawMessage `json:"org.matrix.msc3245.voice"`
		Info     struct {
			MIME string `json:"mimetype"`
			Size int64  `json:"size"`
		} `json:"info"`
		// Membership appears on m.room.member events (invite detection).
		Membership string `json:"membership"`
		// CreateType is the m.room.create content type, which marks a space.
		CreateType string `json:"type"`
		// Name is the m.room.name content, used to label a room the agent was invited to.
		Name     string `json:"name"`
		Mentions struct {
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

func (ev *Event) UnmarshalJSON(b []byte) error {
	type plain Event
	if err := json.Unmarshal(b, (*plain)(ev)); err != nil {
		return err
	}
	ev.Raw = append(ev.Raw[:0], b...)
	return nil
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
	if m.Controller || m.Deleted {
		return
	}
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
	undecryptable []string
	mention       bool
	space         bool
	last          string
	// intro is the join greeting queued when this agent is first invited into the room. It is
	// empty for the agent's own provisioned room, which was not joined by invitation.
	intro string
	turns []*Turn
	// rejects holds event IDs of commands refused as ambiguous in a room shared by several
	// fleet bots; they never become turns, only an outbound explanation.
	rejects []string
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

// otherBots reports whether another enabled agent bot shares this room. Agents track their own
// joined rooms, so this needs no extra Matrix call. The current agent is excluded, because its
// own room entry is only written when this sync window is applied.
func otherBots(st State, id, room string) bool {
	for other, member := range st.Members {
		if other != id && member.Enabled && member.Rooms[room] != nil && !member.Rooms[room].Space {
			return true
		}
	}
	return false
}

// explicitlyMentioned checks Matrix's structured mention metadata, which modern
// Element clients use even when their plain-text body shows only a display name.
func explicitlyMentioned(ev Event, user string) bool {
	for _, u := range ev.Content.Mentions.UserIDs {
		if u == user {
			return true
		}
	}
	return false
}

// leadingAddress recognises a bot's Matrix ID, internal localpart or displayed
// agent name at the beginning of a message. Bare display names require either an
// actual Matrix mention or a colon/comma address ("Iris: !menu"), so ordinary
// prose beginning with "Iris" is not accidentally turned into a command.
func leadingAddress(text, user, name string, explicit bool) (string, bool) {
	local := localUser(user)
	display := strings.TrimPrefix(local, "agent_")
	if name == "" {
		name = display
	}
	prefixes := []struct {
		value       string
		bareDisplay bool
	}{
		{user, false},
		{"@" + local, false},
		{local, false},
		{"@" + name, false},
		{name, true},
	}
	leading := strings.TrimLeft(text, " \t")
	for _, prefix := range prefixes {
		if len(leading) < len(prefix.value) || !strings.EqualFold(leading[:len(prefix.value)], prefix.value) {
			continue
		}
		tail := leading[len(prefix.value):]
		if tail == "" {
			return text, true
		}
		separator := tail[0]
		if separator != ' ' && separator != '\t' && separator != ':' && separator != ',' {
			continue
		}
		if prefix.bareDisplay && !explicit && separator != ':' && separator != ',' {
			continue
		}
		// A colon inside an ID or hostname is not an address separator:
		// "agent_iris:chat.test is slow" must remain ordinary prose.
		if (separator == ':' || separator == ',') && len(tail) > 1 &&
			tail[1] != ' ' && tail[1] != '\t' && tail[1] != '!' {
			continue
		}
		rest := strings.TrimLeft(tail, " \t:,")
		if rest == "" {
			return text, true
		}
		return rest, true
	}
	return text, false
}

// mentioned honours both Matrix mention metadata and the operator-facing agent
// name. The legacy internal-ID substring fallback remains for existing clients.
func mentioned(ev Event, bot Credential, name string) bool {
	if explicitlyMentioned(ev, bot.User) {
		return true
	}
	if _, ok := leadingAddress(ev.Content.Body, bot.User, name, false); ok {
		return true
	}
	la := localUser(bot.User)
	return strings.Contains(ev.Content.Body, bot.User) || strings.Contains(strings.ToLower(ev.Content.Body), la)
}

// stripMention strips supported leading addresses before command detection and
// before passing a normal prompt to Hermes. Element may render a mention using
// the display name alone while recording the bot's ID in m.mentions.
func stripMention(text string, bot Credential, ev Event, name string) string {
	if rest, ok := leadingAddress(text, bot.User, name, explicitlyMentioned(ev, bot.User)); ok {
		return rest
	}
	return text
}

// addressedElsewhere prevents one agent responding to a message visibly
// addressed to another fleet member. Display-name routing uses persisted names;
// internal bot ID addressing is still recognised for other bot namespaces.
func addressedElsewhere(ev Event, bot Credential, st State) bool {
	if parts := strings.SplitN(bot.User, ":", 2); len(parts) == 2 {
		for id, member := range st.Members {
			other := "@agent_" + id + ":" + parts[1]
			if other == bot.User {
				continue
			}
			if _, ok := leadingAddress(ev.Content.Body, other, agentName(member), explicitlyMentioned(ev, other)); ok {
				return true
			}
		}
	}
	own := localUser(bot.User)
	if fields := strings.Fields(ev.Content.Body); len(fields) > 0 {
		word := strings.TrimRight(fields[0], ":,-")
		if strings.HasPrefix(word, "@") {
			if la := localUser(word); la != own && botSender(la) {
				return true
			}
		} else if !strings.Contains(word, ":") && word != own && botSender(word) {
			return true
		}
	}
	other := false
	for _, u := range ev.Content.Mentions.UserIDs {
		if u == bot.User {
			return false
		}
		if botSender(u) {
			other = true
		}
	}
	return other
}

// roomIsSpace reports whether a room is a space container. Sync state answers it for newly
// joined rooms; otherwise the create event is fetched once and cached on the room.
// roomMeta describes a newly joined room: the label used in the join intro, and whether it is a
// space (a container this bot joins but never answers in). Sync state usually carries both; the
// create and name events are fetched only when the state block omits them.
func (mx *Matrix) roomMeta(ctx context.Context, token, room string, state []Event) (string, bool, error) {
	name, space, seenCreate := "", false, false
	for _, ev := range state {
		switch ev.Type {
		case "m.room.create":
			space, seenCreate = ev.Content.CreateType == "m.space", true
		case "m.room.name":
			name = ev.Content.Name
		}
	}
	if !seenCreate {
		var create struct {
			Type string `json:"type"`
		}
		if e := api(ctx, "GET", mx.Base, roomPath(room)+"/state/m.room.create/", token, nil, &create, nil); e != nil {
			return "", false, e
		}
		space = create.Type == "m.space"
	}
	if name == "" && !space {
		var named struct {
			Name string `json:"name"`
		}
		if e := api(ctx, "GET", mx.Base, roomPath(room)+"/state/m.room.name/", token, nil, &named, nil); e != nil && !httpStatus(e, 404) {
			return "", false, e
		}
		name = named.Name
	}
	return roomLabel(name), space, nil
}

// roomIsDirect reports whether a room holds only this bot and one other human member: a direct
// conversation, where requiring a mention would be ceremony. Another agent's bot in the room
// disqualifies it, so two agents never answer every message there. Fails closed: an unreadable
// member list leaves the room mention-required.
func (mx *Matrix) roomIsDirect(ctx context.Context, token, bot, room string) bool {
	var response struct {
		Chunk []Event `json:"chunk"`
	}
	if e := api(ctx, "GET", mx.Base, roomPath(room)+"/members", token, nil, &response, nil); e != nil {
		return false
	}
	others := []string{}
	for _, m := range response.Chunk {
		if m.Content.Membership != "join" || m.StateKey == bot {
			continue
		}
		others = append(others, m.StateKey)
	}
	return len(others) == 1 && !botSender(others[0])
}

// roomLabel renders a room name for the join intro: a single line, length-bounded, or "this
// room" when the room has no usable name. Room names are untrusted input.
func roomLabel(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if len(name) > 64 {
		name = name[:64]
		for len(name) > 0 && !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
	}
	if name == "" {
		return "this room"
	}
	return name
}

// joinIntro is the first message an agent posts where it was invited, so the operator learns what
// the agent does there and how to change it. Membership is the access boundary, so a room the
// operator created starts mention-only.
func joinIntro(room string) string {
	return "Happy to join " + room + ". Reply mode: only when mentioned — send !require_mention off here if you would rather I answer every eligible message. !menu shows the controls."
}

// joinIntroDirect greets a direct conversation, where the agent answers every message: a mention
// would only be ceremony, and the greeting says how to ask for one anyway.
func joinIntroDirect(room string) string {
	return "Happy to join " + room + ". It's just the two of us here, so I answer every message — send !require_mention on here if you would rather I answer only when mentioned. !menu shows the controls."
}

func (mx *Matrix) syncOnce(ctx context.Context, s *Store, id string) error {
	view := s.View()
	m := view.Members[id]
	bot := mx.cred("agent_" + id)
	if m.Room == "" || bot.Token == "" {
		return errors.New("agent room or bot credentials unavailable")
	}
	// The Fleet room admits plain text only while a name prompt is pending, so an
	// operator's reply can name a new agent without being read as a command.
	fleetPrompt := false
	if m.Controller {
		live := s.View()
		fleetPrompt = live.FleetPrompt != nil && live.FleetPrompt.Owner == "@"+mx.c.Admin+":"+mx.c.ServerName && time.Now().Before(live.FleetPrompt.Expires)
	}
	// One sync stream per agent covers its provisioned room plus every room the bot was invited
	// to. Timeline entries are capped per room so a full fleet stays inside the response limit,
	// and no room restriction is set because invites are only visible without one.
	filter := string(jsonBytes(map[string]any{"room": map[string]any{"timeline": map[string]any{"limit": 50}, "ephemeral": map[string]any{"types": []string{}}, "state": map[string]any{"lazy_load_members": true}}, "presence": map[string]any{"types": []string{}}, "account_data": map[string]any{"types": []string{}}}))
	q := url.Values{"timeout": {"25000"}, "filter": {filter}}
	if m.Since != "" {
		q.Set("since", m.Since)
	}
	var raw json.RawMessage
	if e := api(ctx, "GET", mx.Base, "/_matrix/client/v3/sync?"+q.Encode(), bot.Token, nil, &raw, nil); e != nil {
		return e
	}
	var response syncRoomsResponse
	if e := json.Unmarshal(raw, &response); e != nil {
		return errors.New("invalid Matrix sync response")
	}
	if response.Next == "" {
		return errors.New("Matrix sync did not return a cursor")
	}
	mx.acceptInvites(ctx, m, bot, response.Rooms.Invite)
	if e := mx.cryptoSync(ctx, id, raw, m.Since); e != nil {
		return e
	}
	encryptedInbox := append([]EncryptedPending(nil), m.Encrypted...)
	activeInbox := encryptedInbox[:0]
	for _, p := range encryptedInbox {
		if _, left := response.Rooms.Leave[p.Room]; !left {
			activeInbox = append(activeInbox, p)
		}
	}
	encryptedInbox = activeInbox
	for _, p := range encryptedInbox {
		if m.Rooms[p.Room] != nil {
			if _, left := response.Rooms.Leave[p.Room]; !left {
				if response.Rooms.Join == nil {
					response.Rooms.Join = map[string]joinedRoom{}
				}
				if _, exists := response.Rooms.Join[p.Room]; !exists {
					response.Rooms.Join[p.Room] = joinedRoom{}
				}
			}
		}
	}
	scans := map[string]*roomScan{}
	keyRequests := 0
	for room, joined := range response.Rooms.Join {
		if m.Controller && room != m.Room {
			continue
		}
		scan := &roomScan{mention: room != m.Room}
		if known := m.Rooms[room]; known != nil {
			scan.mention, scan.space, scan.last = known.Mention, known.Space, known.LastEvent
		} else {
			label, space, e := mx.roomMeta(ctx, bot.Token, room, joined.State.Events)
			if e != nil {
				return e
			}
			scan.space = space
			// The invite acceptance step is the only place a greeting is posted: the agent's own
			// room was not joined by invitation, and a space is not a conversation.
			if room != m.Room && !space {
				// A room holding only this bot and one other human is a direct conversation, so
				// the agent answers every message there and no mention is required. The default
				// is decided once, when the room is first tracked; the user can change it.
				if mx.roomIsDirect(ctx, bot.Token, bot.User, room) {
					scan.mention = false
					scan.intro = joinIntroDirect(label)
				} else {
					scan.intro = joinIntro(label)
				}
			}
		}
		if room == m.Room {
			scan.mention = false
		}
		if scan.space {
			// A space is a container, not a conversation: join it, never dispatch from it.
			scans[room] = scan
			continue
		}
		if mx.crypto != nil {
			if err := mx.ensureEncrypted(ctx, id, room); err != nil {
				if room == m.Room {
					return err
				}
				continue // An operator-created plaintext room must not wedge the encrypted fleet.
			}
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
			if ev.Type == "m.room.encrypted" {
				found := false
				for _, p := range encryptedInbox {
					var old Event
					_ = json.Unmarshal(p.Raw, &old)
					if old.ID == ev.ID {
						found = true
						break
					}
				}
				if !found {
					encryptedInbox = append(encryptedInbox, EncryptedPending{Room: room, Raw: ev.Raw, Received: time.Now().UTC()})
				}
			}
		}
		// Retry older ciphertext even when this sync has no new timeline entries.
		var keep []EncryptedPending
		for _, p := range encryptedInbox {
			if p.Room != room {
				keep = append(keep, p)
				continue
			}
			var encrypted Event
			if json.Unmarshal(p.Raw, &encrypted) != nil {
				return errors.New("invalid queued ciphertext")
			}
			if botSender(encrypted.Sender) {
				continue
			}
			decrypted, err := mx.decryptEvent(ctx, id, room, p.Raw)
			if err != nil {
				if keyRequests < 8 && p.KeyRequests < 8 && time.Now().After(p.NextKeyRequest) && missingSession(err) {
					mx.requestEventKeys(ctx, id, room, p.Raw)
					keyRequests++
					p.KeyRequests++
					p.NextKeyRequest = time.Now().Add(time.Duration(1<<p.KeyRequests) * time.Minute)
				}
				if time.Since(p.Received) < 7*24*time.Hour {
					keep = append(keep, p)
				} else {
					scan.undecryptable = append(scan.undecryptable, encrypted.ID)
				}
				continue
			}
			events = append(events, decrypted)
		}
		encryptedInbox = keep
		for _, ev := range events {
			if ev.ID == "" {
				continue
			}
			if mx.crypto != nil && ev.Type == "m.room.message" && !ev.Decrypted {
				continue
			}
			if m.Controller && ev.Sender != "@"+mx.c.Admin+":"+mx.c.ServerName {
				continue
			}
			if ev.Type == "m.reaction" && !botSender(ev.Sender) && ev.Content.Relation.Type == "m.annotation" {
				scan.turns = append(scan.turns, &Turn{ID: ev.ID, Room: room, Sender: ev.Sender, MenuID: ev.Content.Relation.ID, Choice: ev.Content.Relation.Key, Text: "!menu-choice", Kind: "command", Phase: "queued", Created: time.Now().UTC()})
				continue
			}
			if botSender(ev.Sender) || ev.Type != "m.room.message" || (ev.Content.MsgType != "m.text" && !mediaMessage(ev.Content.MsgType)) {
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
			t := &Turn{ID: ev.ID, Room: room, Thread: thread, Sender: ev.Sender, Text: stripMention(text, bot, ev, agentName(m)), Phase: "queued", Created: time.Now().UTC()}
			if mediaMessage(ev.Content.MsgType) {
				t.Media = incomingMedia(ev)
				// With filename present, body is a caption; otherwise it is the display filename.
				if ev.Content.Filename == "" {
					t.Text = ""
				}
			}
			// A message addressed to another agent of the fleet is that agent's: stay silent
			// instead of answering it or handing another agent's addressing to Hermes.
			if addressedElsewhere(ev, bot, view) {
				continue
			}
			if t.Media == nil && strings.HasPrefix(strings.TrimSpace(t.Text), "!") {
				t.Kind = "command"
			}
			if m.Controller && (t.Thread != "" || (t.Kind != "command" && !fleetPrompt)) {
				continue
			}
			// A command that does not address this agent is ambiguous once several fleet bots
			// share the room, so refuse it with a pointer rather than acting in every bot at
			// once. !stop stays global; a single-bot room and the agent's own provisioned room
			// never need a mention.
			if t.Kind == "command" && room != m.Room && !mentioned(ev, bot, agentName(m)) &&
				strings.Fields(t.Text)[0] != "!stop" && otherBots(view, id, room) {
				scan.rejects = append(scan.rejects, t.ID)
				continue
			}
			// In a room the operator created the bot behaves like any other member and answers
			// when addressed; in its own room a mention is never required.
			if t.Kind == "" && scan.mention && !mentioned(ev, bot, agentName(m)) {
				continue
			}
			scan.turns = append(scan.turns, t)
		}
		scans[room] = scan
	}
	if len(encryptedInbox) > 1000 || len(jsonBytes(encryptedInbox)) > 16<<20 {
		return errors.New("encrypted inbox full; sync cursor retained")
	}
	return s.Update(func(st *State) error {
		current := st.Members[id]
		if current.Deleted {
			return nil
		}
		if current.Rooms == nil {
			current.Rooms = map[string]*Room{}
		}
		for room, scan := range scans {
			tracked := current.Rooms[room]
			if tracked == nil {
				tracked = &Room{Mention: scan.mention, Space: scan.space}
				current.Rooms[room] = tracked
				if scan.intro != "" {
					// Queued through the reply outbox so the greeting is durable, retried, and
					// delivered exactly once: the room record that carries it is persisted first.
					addNotice(current, room+"-joined", room, "", scan.intro)
				}
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
				} else if (current.Paused || current.Draining) && !fleetSafeControl(t) {
					t.Phase = "done"
					addNotice(current, t.ID+"-paused", room, "", "Agent is paused or draining for a fleet operation; no new work was admitted. Use the Fleet room to check or enable it.")
				} else if !current.Enabled && t.Kind == "" {
					if room != m.Room {
						// A removed agent does not announce itself in operator-created rooms.
						continue
					}
					t.Phase = "done"
					addNotice(current, t.ID+"-disabled", room, t.Thread, "This agent has been removed from the fleet; dispatch is disabled.")
				} else if len(t.Text) > 16<<10 || len(t.Root) > 16<<10 {
					t.Phase = "done"
					addNotice(current, t.ID+"-size", room, t.Thread, "Message or thread root exceeds the 16 KiB text limit; nothing was dispatched.")
				} else if t.Media != nil && t.Media.Error != "" {
					t.Phase = "done"
					addNotice(current, t.ID+"-media", room, t.Thread, t.Media.Error+" Nothing was dispatched.")
				} else if pending(current) >= maxPending && t.Kind == "" {
					t.Phase = "done"
					addNotice(current, t.ID+"-busy", room, t.Thread, "Agent queue is full (100 turns); nothing was dispatched. Try again later.")
				}
				current.Turns = append(current.Turns, t)
			}
			for _, rejected := range scan.rejects {
				addNotice(current, rejected+"-ambiguous", room, "", "Multiple agents are in this room; address the one you want ("+bot.User+" !menu). An unaddressed !stop still stops every agent here.")
			}
			for _, eventID := range scan.undecryptable {
				addNotice(current, eventID+"-undecryptable", room, "", "An encrypted message could not be decrypted or its sender device could not be trusted within seven days. Nothing was dispatched. Verify your device and resend the message.")
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
		current.Encrypted = encryptedInbox
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

// noticeContent builds one text message. A plain notice carries no formatted body, so the
// reader sees the exact characters the bridge composed.
func noticeContent(text string, plain bool) map[string]any {
	content := map[string]any{"msgtype": "m.text", "body": text}
	if !plain {
		content["format"] = "org.matrix.custom.html"
		content["formatted_body"] = matrixFormat(text)
	}
	return content
}

// nextNotice picks the reply to attempt next. The outbox is shared by every room an agent
// serves, so one unwritable room must not stall the others: a room whose head entry is still
// cooling down or has exhausted its attempts is skipped whole, which keeps FIFO order inside
// each room while letting every other room make progress.
func nextNotice(m *Member) *Notice {
	blocked := map[string]bool{}
	for _, n := range m.Outbox {
		room := n.Room
		if room == "" {
			room = m.Room
		}
		if blocked[room] {
			continue
		}
		if time.Now().Before(n.Next) || n.Attempts >= 8 {
			blocked[room] = true
			continue
		}
		return n
	}
	return nil
}

func (mx *Matrix) sendNext(ctx context.Context, s *Store, id string) error {
	m := s.View().Members[id]
	if len(m.Outbox) == 0 {
		return nil
	}
	n := nextNotice(m)
	if n == nil {
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
			removeNotice(st.Members[id], n.ID)
			return nil
		})
	}
	if n.Media != nil {
		return mx.sendMedia(ctx, s, id, room, n)
	}
	parts := chunks(n.Text)
	bot := mx.cred("agent_" + id)
	if n.MenuID != "" && n.EventID != "" {
		return mx.seedMenu(ctx, s, id, n, bot)
	}
	content := noticeContent(parts[n.Sent], n.Plain)
	if n.Thread != "" {
		content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": n.Thread, "is_falling_back": true, "m.in_reply_to": map[string]string{"event_id": n.Thread}}
	}
	txn := stableID(id, n.ID, string(rune(n.Sent)))
	var r struct {
		ID string `json:"event_id"`
	}
	e := mx.sendEncrypted(ctx, s, id, room, n, txn, content, &r)
	if e == nil && r.ID == "" {
		e = errors.New("Matrix send returned no event ID")
	}
	return s.Update(func(st *State) error {
		a := st.Members[id]
		i := noticeIndex(a, n.ID)
		if i < 0 {
			return nil
		}
		v := a.Outbox[i]
		if errors.Is(e, errNoEncryptionRecipient) {
			v.Next = time.Now().Add(30 * time.Second)
			return nil
		}
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
		v.Ciphertext = nil
		if v.Sent == len(parts) {
			a.Outbox = append(a.Outbox[:i], a.Outbox[i+1:]...)
		}
		return nil
	})
}

func (mx *Matrix) seedMenu(ctx context.Context, s *Store, id string, notice *Notice, bot Credential) error {
	for {
		m := s.View().Members[id]
		i := noticeIndex(m, notice.ID)
		if i < 0 {
			return nil
		}
		n := m.Outbox[i]
		menu := m.Menus[n.MenuID]
		if menu == nil || !menuValid(m, menu) || n.Reaction >= len(menu.Options) {
			return s.Update(func(st *State) error {
				removeNotice(st.Members[id], n.ID)
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
			j := noticeIndex(a, n.ID)
			if j < 0 {
				return nil
			}
			v := a.Outbox[j]
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
