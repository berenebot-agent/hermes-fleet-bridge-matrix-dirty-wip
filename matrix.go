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
	return s.Update(func(st *State) error { st.Members[id].Room = r.Room; return nil })
}

type Event struct {
	ID      string `json:"event_id"`
	Sender  string `json:"sender"`
	Type    string `json:"type"`
	Content struct {
		Body     string `json:"body"`
		MsgType  string `json:"msgtype"`
		Relation struct {
			Type  string `json:"rel_type"`
			ID    string `json:"event_id"`
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

func (mx *Matrix) syncOnce(ctx context.Context, s *Store, id string) error {
	m := s.View().Members[id]
	bot := mx.cred("agent_" + id)
	filter := string(jsonBytes(map[string]any{"room": map[string]any{"rooms": []string{m.Room}, "timeline": map[string]int{"limit": 100}, "ephemeral": map[string]any{"types": []string{}}, "state": map[string]any{"lazy_load_members": true}}, "presence": map[string]any{"types": []string{}}, "account_data": map[string]any{"types": []string{}}}))
	q := url.Values{"timeout": {"25000"}, "filter": {filter}}
	if m.Since != "" {
		q.Set("since", m.Since)
	}
	var response struct {
		Next  string `json:"next_batch"`
		Rooms struct {
			Join map[string]struct {
				Timeline Timeline `json:"timeline"`
			} `json:"join"`
		} `json:"rooms"`
	}
	if e := api(ctx, "GET", mx.Base, "/_matrix/client/v3/sync?"+q.Encode(), bot.Token, nil, &response, nil); e != nil {
		return e
	}
	if response.Next == "" {
		return errors.New("Matrix sync did not return a cursor")
	}
	timeline := response.Rooms.Join[m.Room].Timeline
	events := timeline.Events
	if timeline.Limited {
		prefix, e := mx.backfill(ctx, m, bot.Token, timeline.Prev)
		if e != nil {
			return e
		}
		events = append(prefix, events...)
	}
	roots := map[string]string{}
	for _, ev := range events {
		if ev.Sender != mx.cred(mx.c.Admin).User || ev.Type != "m.room.message" || ev.Content.MsgType != "m.text" || ev.Content.Relation.Type != "m.thread" {
			continue
		}
		root := ev.Content.Relation.ID
		if root == "" || len(root) > 255 {
			return errors.New("invalid Matrix thread relation")
		}
		var origin Event
		if e := api(ctx, "GET", mx.Base, roomPath(m.Room)+"/event/"+url.PathEscape(root), bot.Token, nil, &origin, nil); e != nil {
			return errors.New("cannot validate thread root; sync cursor retained")
		}
		if origin.Type != "m.room.message" || (origin.Sender != bot.User && origin.Sender != mx.cred(mx.c.Admin).User) {
			return errors.New("thread root is not an approved conversation event")
		}
		roots[root] = origin.Content.Body
	}
	return s.Update(func(st *State) error {
		current := st.Members[id]
		for _, ev := range events {
			if ev.ID == "" {
				continue
			}
			current.LastEvent = ev.ID
			if ev.Sender != mx.cred(mx.c.Admin).User || ev.Type != "m.room.message" || ev.Content.MsgType != "m.text" || findTurn(current, ev.ID) != nil {
				continue
			}
			if ev.Content.Relation.Type != "" && ev.Content.Relation.Type != "m.thread" {
				continue
			}
			thread := ""
			if ev.Content.Relation.Type == "m.thread" {
				thread = ev.Content.Relation.ID
			}
			text := ev.Content.Body
			if ev.Content.Relation.Reply != nil && strings.HasPrefix(text, ">") {
				if pos := strings.Index(text, "\n\n"); pos >= 0 {
					text = text[pos+2:]
				}
			}
			t := &Turn{ID: ev.ID, Thread: thread, Text: text, Root: roots[thread], Phase: "queued", Created: time.Now().UTC()}
			if strings.HasPrefix(strings.TrimSpace(text), "!") {
				t.Kind = "command"
			}
			if !current.Enabled && t.Kind == "" {
				t.Phase = "done"
				addNotice(current, t.ID+"-disabled", thread, "This agent has been removed from HERMES_DIRS; dispatch is disabled.")
			} else if len(text) > 16<<10 || len(t.Root) > 16<<10 {
				t.Phase = "done"
				addNotice(current, t.ID+"-size", thread, "Message or thread root exceeds the 16 KiB text limit; nothing was dispatched.")
			} else if pending(current) >= maxPending && t.Kind == "" {
				t.Phase = "done"
				addNotice(current, t.ID+"-busy", thread, "Agent queue is full (100 turns); nothing was dispatched. Try again later.")
			}
			current.Turns = append(current.Turns, t)
		}
		current.Since = response.Next
		prune(current)
		return nil
	})
}
func (mx *Matrix) backfill(ctx context.Context, m *Member, token, from string) ([]Event, error) {
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
		if e := api(ctx, "GET", mx.Base, roomPath(m.Room)+"/messages?"+q.Encode(), token, nil, &r, nil); e != nil {
			return nil, e
		}
		found := false
		for _, ev := range r.Chunk {
			if ev.ID == m.LastEvent && m.LastEvent != "" {
				found = true
				break
			}
			reverse = append(reverse, ev)
		}
		if found || len(r.Chunk) == 0 || r.End == "" || r.End == from {
			if !found && m.LastEvent != "" {
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
	parts := chunks(n.Text)
	bot := mx.cred("agent_" + id)
	content := map[string]any{"msgtype": "m.text", "body": parts[n.Sent]}
	if n.Thread != "" {
		content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": n.Thread, "is_falling_back": true, "m.in_reply_to": map[string]string{"event_id": n.Thread}}
	}
	txn := stableID(id, n.ID, string(rune(n.Sent)))
	var r struct {
		ID string `json:"event_id"`
	}
	e := api(ctx, "PUT", mx.Base, roomPath(m.Room)+"/send/m.room.message/"+txn, bot.Token, content, &r, nil)
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
		v.Sent++
		if v.Sent == len(parts) {
			a.Outbox = a.Outbox[1:]
		}
		return nil
	})
}
func (mx *Matrix) typing(ctx context.Context, m *Member, on bool) {
	bot := mx.cred("agent_" + m.ID)
	_ = api(ctx, "PUT", mx.Base, roomPath(m.Room)+"/typing/"+url.PathEscape(bot.User), bot.Token, map[string]any{"typing": on, "timeout": 25000}, nil, nil)
}
