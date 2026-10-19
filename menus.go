package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const menuPageSize = 5
const maxConversations = 1000

var menuKeys = []string{"1️⃣", "2️⃣", "3️⃣", "4️⃣", "5️⃣"}

// Menus are local, single-use capabilities, not commands supplied by Matrix text.
type Menu struct {
	Room, Owner, EventID, Session string
	ApprovalTurn                  string
	Revision                      uint64
	Created                       time.Time
	Used                          bool
	Options                       []MenuOption
}

type MenuOption struct {
	Key, Label, Action, Value, Provider string
}

func selectedKey(m *Member, room string) string {
	if r := m.Rooms[room]; r != nil && r.Selected != "" {
		return r.Selected
	}
	return threadKey("")
}

func shortText(s string, limit int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return string(r)
}

func expireMenus(m *Member, room string) {
	if r := m.Rooms[room]; r != nil {
		r.Revision++
	}
	for _, menu := range m.Menus {
		if menu.Room == room && menu.ApprovalTurn == "" {
			menu.Used = true
		}
	}
}

func menuValid(m *Member, menu *Menu) bool {
	r := m.Rooms[menu.Room]
	if menu.ApprovalTurn != "" {
		t := findTurn(m, menu.ApprovalTurn)
		return m.Enabled && r != nil && !menu.Used && t != nil && t.Phase == "running" && !t.Stop &&
			len(menu.Options) > 0 && t.Approval == menu.Options[0].Value && menu.Session == selectedKey(m, menu.Room)
	}
	return m.Enabled && r != nil && !r.Space && !menu.Used && time.Since(menu.Created) < 24*time.Hour &&
		menu.Revision == r.Revision && menu.Session == selectedKey(m, menu.Room)
}

func postMenu(m *Member, id, room, owner, title string, options []MenuOption, replace bool) {
	if r := m.Rooms[room]; r == nil || r.Space {
		return
	}
	if m.Menus == nil {
		m.Menus = map[string]*Menu{}
	}
	if replace {
		expireMenus(m, room)
	}
	// Outbox references retain unsent menus; everything else expires after one day.
	pending := map[string]bool{}
	for _, n := range m.Outbox {
		pending[n.MenuID] = true
	}
	for key, menu := range m.Menus {
		if !pending[key] && time.Since(menu.Created) >= 24*time.Hour && (menu.ApprovalTurn == "" || !menuValid(m, menu)) {
			delete(m.Menus, key)
		}
	}
	if len(m.Menus) >= maxRooms+maxPending {
		oldest := ""
		for key, menu := range m.Menus {
			if menu.Used && !pending[key] && (oldest == "" || menu.Created.Before(m.Menus[oldest].Created)) {
				oldest = key
			}
		}
		if oldest == "" {
			addNotice(m, id, room, "", title+"\nToo many pending menus. Use direct commands or finish existing work and retry !menu.")
			return
		}
		delete(m.Menus, oldest)
	}
	text := title
	for i := range options {
		if options[i].Key == "" {
			options[i].Key = menuKeys[i]
		}
		text += "\n" + options[i].Key + " " + options[i].Label
	}
	text += "\nTap a reaction below. Menus are single-use; !menu opens fresh controls."
	m.Menus[id] = &Menu{Room: room, Owner: owner, Session: selectedKey(m, room), Revision: m.Rooms[room].Revision, Created: time.Now().UTC(), Options: options}
	addNotice(m, id, room, "", text)
	m.Outbox[len(m.Outbox)-1].MenuID = id
}

func homeMenu(m *Member, t *Turn) {
	room := turnRoom(m, t)
	s := getSession(m, room, "")
	title := "Agent controls · " + m.ID + "\nConversation: " + conversationTitle(s)
	if s.Model != "" {
		title += "\nModel: " + s.Model
		if s.Provider != "" {
			title += " [" + s.Provider + "]"
		}
	}
	postMenu(m, t.ID+"-menu", room, t.Sender, title, []MenuOption{
		{Label: "Status", Action: "status"},
		{Label: "Model / provider", Action: "models"},
		{Label: "Conversations · New / Resume", Action: "conversations"},
		{Label: "Context / usage", Action: "context-menu"},
		{Label: "More · Reply mode / Stop / Help", Action: "more"},
	}, true)
}

func conversationTitle(s *Session) string {
	if s.Title != "" {
		return s.Title
	}
	return "Untitled conversation"
}

func roomIdle(m *Member, room string) bool {
	for _, t := range m.Turns {
		if turnRoom(m, t) == room && t.Phase != "done" {
			return false
		}
	}
	for _, n := range m.Outbox {
		if n.Room == room && n.MenuID == "" {
			return false
		}
	}
	return true
}

func pageOptions(all []MenuOption, page int, action, query string) []MenuOption {
	page = max(0, min(page, max(0, (len(all)-1)/menuPageSize)))
	start := page * menuPageSize
	options := append([]MenuOption(nil), all[start:min(start+menuPageSize, len(all))]...)
	if page > 0 {
		options = append(options, MenuOption{Key: "⬅️", Label: "Previous", Action: action, Value: strconv.Itoa(page - 1), Provider: query})
	}
	if start+menuPageSize < len(all) {
		options = append(options, MenuOption{Key: "➡️", Label: "Next", Action: action, Value: strconv.Itoa(page + 1), Provider: query})
	}
	return append(options, MenuOption{Key: "❌", Label: "Cancel", Action: "cancel"})
}

// resolveMenu persists the chosen action before doing any remote work, so a restart cannot
// reinterpret a numbered choice against a different menu or selected conversation.
func (e *Engine) resolveMenu(id, turn string) bool {
	ok := false
	_ = e.s.Update(func(st *State) error {
		m := st.Members[id]
		t := findTurn(m, turn)
		if t.Selection != nil {
			ok = true
			return nil
		}
		menu := m.Menus[t.MenuID]
		if menu == nil || menu.Owner != t.Sender || menu.Room != turnRoom(m, t) {
			t.Phase = "done"
			return nil
		}
		if !menuValid(m, menu) {
			t.Phase = "done"
			addNotice(m, t.ID+"-stale", menu.Room, "", "This menu is closed or stale. Send !menu for fresh controls.")
			return nil
		}
		for _, option := range menu.Options {
			if option.Key == t.Choice {
				menu.Used = true
				t.Selection = &option
				t.MenuRevision = menu.Revision
				ok = true
				break
			}
		}
		if !ok {
			t.Phase = "done"
		}
		return nil
	})
	return ok
}

// menuCommand handles local mutations atomically with their reply and completion marker.
// Only model inventory reads need network access; stop/approvals use the existing handlers.
func (e *Engine) menuCommand(ctx context.Context, id, turn string) bool {
	t := findTurn(e.s.View().Members[id], turn)
	fields := strings.Fields(t.Text)
	if len(fields) == 0 {
		return false
	}
	action, value, provider := "", "", ""
	page := 0
	if t.Selection != nil {
		action, value, provider = t.Selection.Action, t.Selection.Value, t.Selection.Provider
		if action == "models-page" || action == "resume-page" {
			page, _ = strconv.Atoi(value)
			value = provider
		}
	} else {
		switch fields[0] {
		case "!menu", "!help":
			action = "home"
		case "!new", "!reset":
			action = "new"
		case "!conversations", "!resume":
			action = "resume-page"
			value = strings.Join(fields[1:], " ")
		case "!models":
			action = "models"
			value = strings.Join(fields[1:], " ")
		case "!model":
			if len(fields) == 2 || len(fields) == 3 {
				action, value = "model-scope", fields[1]
				if len(fields) == 3 {
					provider = fields[2]
				}
			}
		case "!require_mention":
			action = "reply-mode"
			if len(fields) == 2 {
				value = fields[1]
			}
		case "!context":
			action = "context"
		}
	}
	if action == "" {
		return false
	}
	if action == "status" || action == "stop" || action == "usage" || action == "approve" || action == "deny" {
		_ = e.s.Update(func(st *State) error {
			v := findTurn(st.Members[id], turn)
			v.Text = "!" + action
			if action == "approve" || action == "deny" {
				v.Text += " " + value
			}
			v.Selection = nil
			v.MenuID = ""
			return nil
		})
		return false
	}
	var models []MenuOption
	modelError := ""
	snapshot := e.s.View().Members[id]
	room := turnRoom(snapshot, t)
	var revision uint64
	if r := snapshot.Rooms[room]; r != nil {
		revision = r.Revision
	}
	if (action == "models" || action == "models-page") && snapshot.Enabled && snapshot.Rooms[room] != nil && !snapshot.Rooms[room].Space {
		models, modelError = e.modelOptions(ctx, id, room, value)
	}
	_ = e.s.Update(func(st *State) error {
		m := st.Members[id]
		v := findTurn(m, turn)
		if v.Phase == "done" {
			return nil
		}
		room := turnRoom(m, v)
		r := m.Rooms[room]
		reply := ""
		v.Phase = "done"
		if r == nil || r.Space || !m.Enabled {
			addNotice(m, v.ID+"-command", room, "", "Agent or room is no longer available; nothing changed.")
			return nil
		}
		if (v.Selection != nil && v.MenuRevision != r.Revision) || ((action == "models" || action == "models-page") && revision != r.Revision) {
			addNotice(m, v.ID+"-command", room, "", "This menu is stale. Send !menu for fresh controls.")
			return nil
		}
		post := func(title string, options []MenuOption) {
			postMenu(m, v.ID+"-menu", room, v.Sender, title, options, true)
		}
		switch action {
		case "home":
			homeMenu(m, v)
		case "cancel":
			reply = "Menu closed."
		case "more":
			post("More controls", []MenuOption{{Label: "Reply mode", Action: "reply-menu"}, {Label: "Stop this conversation", Action: "stop"}, {Label: "Command help", Action: "help-text"}})
		case "help-text":
			reply = helpText
		case "context-menu":
			post("Context and usage", []MenuOption{{Label: "Context window (API limitation)", Action: "context"}, {Label: "Cumulative usage (may wake agent)", Action: "usage"}})
		case "context":
			reply = "Hermes has a native /context report, but its supported HTTP API does not expose that report to this bridge. Context-window occupancy/headroom is unavailable here. Use !usage for verified cumulative session counters; those are not context occupancy."
		case "reply-menu":
			if room == m.Room {
				reply = "Reply mode: Always responds — dedicated room. Eligible messages never require a mention. Commands and menu reactions remain available without mentions."
			} else {
				mode := "Every eligible message"
				if r.Mention {
					mode = "Only when mentioned"
				}
				post("Reply mode for this agent in this room: "+mode+"\nCommands and menu reactions do not require mentions.", []MenuOption{{Label: "Only when mentioned", Action: "reply-mode", Value: "on"}, {Label: "Every eligible message", Action: "reply-mode", Value: "off"}})
			}
		case "reply-mode":
			if value != "on" && value != "off" {
				reply = "Usage: !require_mention on|off."
			} else if room == m.Room {
				r.Mention = false
				reply = "Always responds — dedicated room. Mentions cannot be required here."
			} else {
				r.Mention = value == "on"
				expireMenus(m, room)
				if r.Mention {
					reply = "Mentions are now required in this room. Commands and menu reactions still work without one."
				} else {
					reply = "Mentions are no longer required in this room: every eligible message is dispatched to this agent."
				}
			}
		case "conversations":
			post("Conversations · "+conversationTitle(getSession(m, room, "")), []MenuOption{{Label: "New conversation (keep previous)", Action: "new"}, {Label: "Resume conversation", Action: "resume-page"}})
		case "new", "resume":
			if !roomIdle(m, room) {
				reply = "Cannot switch while this room has queued, running, unresolved work or undelivered replies. Use !stop where applicable, wait for settlement/delivery, then try again."
				break
			}
			getSession(m, room, "")
			if action == "new" {
				if len(m.Sessions[room]) >= maxConversations {
					reply = "Conversation storage limit reached; existing history was preserved."
					break
				}
				value = "chat:" + stableID(room, v.ID)
				if m.Sessions[room][value] == nil {
					m.Sessions[room][value] = &Session{ID: "matrix_" + stableID(room, value), Model: m.DefaultModel, Provider: m.DefaultProvider, LastUsed: time.Now().UTC()}
				}
			} else if m.Sessions[room][value] == nil || (value != "room" && !strings.HasPrefix(value, "chat:")) {
				reply = "Conversation is no longer available; nothing changed."
				break
			}
			r.Selected = value
			getSession(m, room, "").LastUsed = time.Now().UTC()
			expireMenus(m, room)
			reply = "Selected conversation: " + conversationTitle(getSession(m, room, "")) + ". Matrix history is unchanged; future messages use this conversation."
		case "resume-page":
			getSession(m, room, "")
			keys := []string{}
			for key, session := range m.Sessions[room] {
				if session != nil && (key == "room" || strings.HasPrefix(key, "chat:")) && strings.Contains(strings.ToLower(conversationTitle(session)), strings.ToLower(value)) {
					keys = append(keys, key)
				}
			}
			sort.Slice(keys, func(i, j int) bool {
				a, b := m.Sessions[room][keys[i]], m.Sessions[room][keys[j]]
				if a.LastUsed.Equal(b.LastUsed) {
					return keys[i] < keys[j]
				}
				return a.LastUsed.After(b.LastUsed)
			})
			all := []MenuOption{}
			for _, key := range keys {
				s := m.Sessions[room][key]
				label := conversationTitle(s)
				if key == selectedKey(m, room) {
					label += " (selected)"
				}
				if !s.LastUsed.IsZero() {
					label += " · " + s.LastUsed.UTC().Format("2006-01-02 15:04 UTC")
				}
				all = append(all, MenuOption{Label: label, Action: "resume", Value: key})
			}
			post("Resume · !resume <text> filters by first prompt", pageOptions(all, page, "resume-page", value))
		case "models", "models-page":
			if modelError != "" {
				reply = modelError
			} else {
				post("Models / providers · !models <text> filters · page "+strconv.Itoa(page+1), pageOptions(models, page, "models-page", value))
			}
		case "model-scope":
			if len(value) == 0 || len(value) > 256 || len(provider) > 80 {
				reply = "Invalid model/provider selection."
				break
			}
			post("Select scope for "+shortText(value, 100)+" ["+shortText(provider, 80)+"]\nGlobal means this agent’s bridge-managed chats/default only, not Hermes global configuration. Submitted runs are unchanged.", []MenuOption{{Label: "This chat", Action: "model-chat", Value: value, Provider: provider}, {Label: "Global · all bridge chats for this agent", Action: "model-global", Value: value, Provider: provider}, {Key: "❌", Label: "Cancel", Action: "cancel"}})
		case "model-chat", "model-global":
			if action == "model-global" {
				m.DefaultModel, m.DefaultProvider = value, provider
				for _, sessions := range m.Sessions {
					for _, s := range sessions {
						s.Model, s.Provider = value, provider
					}
				}
				for room := range m.Rooms {
					expireMenus(m, room)
				}
			} else {
				s := getSession(m, room, "")
				s.Model, s.Provider = value, provider
				expireMenus(m, room)
			}
			reply = "Model selection saved: " + value + " [" + provider + "]. Scope: " + strings.TrimPrefix(action, "model-") + ". Submitted runs are unchanged; Hermes validates availability on the next run."
		default:
			reply = "Unsupported menu action; nothing was dispatched."
		}
		if reply != "" {
			addNotice(m, v.ID+"-command", room, "", reply)
		}
		for _, n := range m.Outbox {
			if strings.HasPrefix(n.ID, v.ID+"-") {
				n.Text = e.redact(n.Text)
			}
		}
		return nil
	})
	return true
}

func (e *Engine) modelOptions(ctx context.Context, id, room, query string) ([]MenuOption, string) {
	endpoint, _, err := e.ensure(ctx, id, room)
	if err != nil {
		return nil, "Cannot reach a ready Hermes API. Check the agent configuration locally."
	}
	var catalog struct {
		Providers []struct {
			Slug   string   `json:"slug"`
			Models []string `json:"models"`
		} `json:"providers"`
	}
	all := []MenuOption{}
	add := func(model, provider string) {
		if model == "" || len(model) > 256 || len(provider) > 80 || len(all) >= 1000 {
			return
		}
		all = append(all, MenuOption{Label: shortText(fmt.Sprintf("%s [%s]", model, provider), 150), Action: "model-scope", Value: model, Provider: provider})
	}
	if api(ctx, "GET", endpoint, "/api/model/options?include_unconfigured=false", e.mx.key(id), nil, &catalog, nil) == nil {
		for _, p := range catalog.Providers {
			for _, model := range p.Models {
				add(model, p.Slug)
			}
		}
	}
	if len(all) == 0 {
		var routes struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if api(ctx, "GET", endpoint, "/v1/models", e.mx.key(id), nil, &routes, nil) != nil {
			return nil, "Model listing unavailable on this Hermes API."
		}
		for _, model := range routes.Data {
			add(model.ID, "")
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Label < all[j].Label })
	filtered := []MenuOption{}
	for _, option := range all {
		if strings.Contains(strings.ToLower(option.Value+" "+option.Provider), strings.ToLower(query)) {
			filtered = append(filtered, option)
		}
	}
	if len(filtered) == 0 {
		return nil, "No matching configured models or advertised routes. Try !models with a different filter."
	}
	return filtered, ""
}
