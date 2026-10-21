package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const fleetID = "fleet-controller"

// Name prompts ask the operator for a destination name; the answer is their next
// plain message in the Fleet room.
const (
	promptCopy     = "prompt-copy"
	promptTemplate = "prompt-template"
	promptCreate   = "prompt-create"
	promptRename   = "prompt-rename"
)

var errDeactivationUnavailable = errors.New("Matrix account deactivation unconfirmed")

// errAgentUnreachable marks an agent whose API cannot be reached at all. It is different from
// an agent that is merely busy: a busy agent will finish, an unreachable one never will.
var errAgentUnreachable = errors.New("the agent's API is unreachable, so its activity cannot be proven")

// errAgentBusy marks an agent that answered but still has work in flight. It is a normal
// wait, not a fault, so it never holds the operation.
var errAgentBusy = errors.New("the agent still has work in flight")

type FleetConfirmation struct {
	Action, Target, Name, Source, Code, Owner string
	Expires                                   time.Time
}

// One durable operation at a time keeps ownership, draining and recovery unambiguous.
type FleetOperation struct {
	ID, Action, Source, Name, Target, Stage, Error string
	WasRunning, WasPaused                          bool
	Port                                           int
	// Forced marks a delete that could not drain through the agent's API and was
	// explicitly confirmed by the operator. It relaxes the drain proof, never the
	// storage boundaries.
	Forced bool
}

func agentName(m *Member) string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

func fleetSafeControl(t *Turn) bool {
	if t.Kind != "command" {
		return false
	}
	f := strings.Fields(t.Text)
	if len(f) == 0 {
		return false
	}
	switch f[0] {
	case "!menu", "!help", "!status", "!stop", "!approve", "!deny", "!discard", "!resend":
		return true
	}
	return false
}

func fleetMember(st State, name string) *Member {
	for _, m := range st.Members {
		if m.Managed && !m.Deleted && agentName(m) == name {
			return m
		}
	}
	return nil
}

func reconcileFleet(ctx context.Context, c Config, s *Store, mx *Matrix, d *Docker) error {
	if err := fleetStorage(c); err != nil {
		return err
	}
	view := s.View()
	if view.FleetDir != "" && (view.FleetDir != c.FleetDir || view.TemplateDir != c.TemplateDir) {
		return errors.New("fleet storage identity changed")
	}
	dirs := map[string]string{}
	for id, m := range view.Members {
		if m.Enabled && !m.Controller && !m.Deleted && !m.Managed {
			dirs[m.Dir] = id
		}
		if m.Managed && !m.Deleted {
			dirs[m.Dir] = id
		}
	}
	entries, err := os.ReadDir(c.FleetDir)
	if err != nil {
		return errors.New("cannot scan fleet directory")
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue // Hidden staging directories belong to operation recovery, not discovery.
		}
		dir := filepath.Join(c.FleetDir, name)
		if dirs[dir] != "" {
			continue
		}
		if !entry.IsDir() || !fleetName(name) {
			return fmt.Errorf("unsafe fleet entry %s: expected an agent directory", name)
		}
		if op := view.FleetOperation; op != nil && op.Action != "save-template" && op.Name == name {
			continue
		}
		deleted := false
		for _, m := range view.Members {
			if m.Deleted && m.Dir == dir {
				deleted = true
			}
		}
		if !deleted {
			id, err := fleetDirectoryIdentity(dir)
			if err != nil {
				return fmt.Errorf("agent %s: %w", name, err)
			}
			if other := view.Members[id]; other != nil {
				return fmt.Errorf("agent %s: identity already belongs to another or deleted member", name)
			}
			for other, existing := range dirs {
				if existing == id && other != dir {
					return errors.New("fleet directories share an agent identity")
				}
			}
			retiredName := false
			for _, retired := range view.Members {
				if retired.Deleted && agentName(retired) == name {
					retiredName = true
				}
			}
			if retiredName {
				return fmt.Errorf("agent %s: name belongs to a deleted member; use Fleet to confirm name reuse", name)
			}
			dirs[dir] = id
		}
	}
	projects := map[string]bool{}
	ports := map[int]bool{}
	var discovered []*Member
	names := map[string]string{}
	for id, m := range view.Members {
		if m.Managed && !m.Deleted {
			names[agentName(m)] = id
		}
	}
	for dir, id := range dirs {
		if id == fleetID {
			return errors.New("fleet-controller is reserved for the Fleet room")
		}
		old := view.Members[id]
		if op := view.FleetOperation; op != nil && op.Source == id && op.Action == "delete" && op.Stage == "deleting" {
			continue
		}
		o := c.Overrides[filepath.Base(dir)]
		if old != nil {
			o = Override{Service: old.Service, Project: old.Project, Port: old.Port, Network: old.Network}
		} else if o.Port == 0 {
			port, err := fleetDirectoryPort(dir)
			if err != nil {
				return fmt.Errorf("agent %s: %w", filepath.Base(dir), err)
			}
			o.Port = port
		}
		m, err := d.discover(ctx, dir, o)
		if err != nil {
			return fmt.Errorf("agent %s: %w", filepath.Base(dir), err)
		}
		m.ID, m.Name, m.Managed = id, filepath.Base(dir), true
		if old == nil {
			if err = restoreFleetDirectorySettings(m); err != nil {
				return err
			}
		}
		if old != nil && old.Name != "" {
			m.Name = old.Name
		}
		if other := names[m.Name]; other != "" && other != id {
			return errors.New("managed agent display name is already in use")
		}
		names[m.Name] = id
		if err = d.validateManaged(ctx, c, m); err != nil {
			return err
		}
		if projects[m.Project] || (m.Network == "host" && ports[m.Port]) {
			return errors.New("managed agents share a Compose project or host API port")
		}
		projects[m.Project], ports[m.Port] = true, true
		if old != nil && pending(old) > 0 && (old.Dir != m.Dir || old.Project != m.Project || old.Service != m.Service || old.Port != m.Port || old.Network != m.Network) {
			return errors.New("managed agent identity changed with outstanding work")
		}
		if err = d.checkProject(ctx, m); err != nil {
			return err
		}
		discovered = append(discovered, m)
	}
	for _, m := range discovered {
		host := "0.0.0.0"
		if m.Network == "host" {
			host = "127.0.0.1"
		}
		key, err := configureEnv(filepath.Join(m.Home, ".env"), host, m.Port)
		if err != nil {
			return err
		}
		if err = mx.rememberKey(m.ID, key); err != nil {
			return err
		}
		if err = s.Update(func(st *State) error {
			if prev := st.Members[m.ID]; prev != nil {
				prev.Managed, prev.Name, prev.Enabled = true, m.Name, true
				prev.Dir, prev.Home, prev.Project, prev.Service, prev.Port, prev.Network, prev.Files = m.Dir, m.Home, m.Project, m.Service, m.Port, m.Network, m.Files
			} else {
				st.Members[m.ID] = m
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return s.Update(func(st *State) error {
		st.FleetDir, st.TemplateDir = c.FleetDir, c.TemplateDir
		if m := st.Members[fleetID]; m != nil {
			if !m.Controller {
				return errors.New("fleet controller identity is already in use")
			}
		} else {
			st.Members[fleetID] = &Member{ID: fleetID, Name: "Fleet", Controller: true, Enabled: true, Rooms: map[string]*Room{}, Sessions: map[string]map[string]*Session{}}
		}
		return nil
	})
}

const fleetHelp = `Fleet controls (operator only, in this room):
!menu / !fleet — open Fleet controls
!fleet agents / !fleet templates
!fleet status <agent>
!fleet start <agent> / !fleet shutdown <agent>
!fleet pause <agent> / !fleet enable <agent>
!fleet create <name> from <template>
!fleet copy <agent> to <name>
!fleet save-template <agent> as <template>
!fleet rename <agent> to <name>
!fleet delete <agent>
!fleet force-delete <agent> — delete without draining, when a normal delete cannot complete
!fleet cancel — cancel a waiting operation, confirmation or name prompt
!fleet retry — retry a held operation after fixing the reported problem

Rename agent, Copy agent, Save as template and the template list ask for the new name: reply with
just the name. An invalid name keeps the prompt alive; !fleet cancel ends it.
Rename changes the bot display name and dedicated room name, not directories, containers or history.
A shared room is named "<agent> — <session name>", so a rename moves the agent part of those names too.

Shutdown allows the next message to wake the agent. Pause drains work and shuts down, preventing wake until enabled. Copies and agents created from templates start ready to receive work. Delete permanently erases agent files and the agent's own Matrix room, not templates or other copies. Templates can contain private data and provider credentials. No secrets should be pasted into this room.`

func fleetReply(st *State, t *Turn, text string) {
	m := st.Members[fleetID]
	addNotice(m, t.ID+"-fleet", m.Room, "", text)
	t.Phase = "done"
}

func fleetMenu(st *State, t *Turn, action, value, extra string) {
	m := st.Members[fleetID]
	options := []MenuOption{}
	title := "Fleet"
	switch action {
	case "agents", "templates":
		page, _ := strconv.Atoi(value)
		if action == "agents" {
			for id, a := range st.Members {
				if a.Managed && !a.Deleted {
					options = append(options, MenuOption{Label: agentName(a) + " · " + fleetAvailability(a), Action: "agent", Value: id})
				}
			}
		} else {
			for _, tpl := range listFleetTemplates(extra) {
				options = append(options, MenuOption{Label: tpl.Name + " · " + tpl.Kind, Action: "template", Value: tpl.Name})
			}
		}
		sort.Slice(options, func(i, j int) bool { return options[i].Label < options[j].Label })
		title += " · " + action
		options = pageOptions(options, page, action, "")
	case "agent", "more":
		a := st.Members[value]
		if a == nil || !a.Managed || a.Deleted {
			fleetReply(st, t, "Agent is no longer available.")
			return
		}
		title = agentName(a) + " · " + fleetAvailability(a) + fmt.Sprintf("\nPending turns: %d", pending(a))
		availability := MenuOption{Label: "Pause", Action: "pause", Value: value}
		if a.Paused {
			availability.Label, availability.Action = "Enable", "enable"
		}
		options = []MenuOption{
			{Label: "Refresh status", Action: "status", Value: value},
			{Label: "Rename agent", Action: promptRename, Value: value},
			{Label: "More · Lifecycle", Action: "more", Value: value},
			{Label: "Copy agent", Action: promptCopy, Value: value},
			{Label: "Save as template", Action: promptTemplate, Value: value},
			{Label: "Back", Action: "home"},
		}
		if action == "more" {
			title = "Lifecycle · " + agentName(a) + " · " + fleetAvailability(a)
			options = []MenuOption{
				{Label: "Start", Action: "start", Value: value},
				{Label: "Shutdown", Action: "shutdown", Value: value}, availability,
				{Label: "Delete", Action: "delete", Value: value},
				{Label: "Force delete (no drain)", Action: "force-delete", Value: value},
				{Label: "Back to agent", Action: "agent", Value: value},
			}
		}
	case "template":
		tpl, err := readFleetTemplate(extra, value)
		if err != nil {
			fleetReply(st, t, err.Error())
			return
		}
		fleetStartPrompt(st, t, promptCreate, tpl.Name, "Create from "+tpl.Name+" → reply with the new agent name.", "!fleet create <new-name> from "+tpl.Name)
		return
	default:
		options = []MenuOption{{Label: "Agents", Action: "agents"}, {Label: "Templates", Action: "templates"}, {Label: "Help", Action: "help"}}
		if op := st.FleetOperation; op != nil {
			title += "\nOperation: " + op.Action + " · " + op.Stage
			if op.Error != "" {
				title += "\n" + op.Error
				options = append(options, MenuOption{Label: "Retry operation", Action: "retry"})
			}
			if op.Stage == "waiting" {
				options = append(options, MenuOption{Label: "Cancel pending operation", Action: "cancel"})
			}
		}
	}
	postMenu(m, t.ID+"-fleet-menu", m.Room, t.Sender, title, options, true)
	t.Phase = "done"
}

func fleetAvailability(m *Member) string {
	if m.Draining {
		return "Draining"
	}
	if m.Paused {
		return "Paused"
	}
	return "Available"
}

func (e *Engine) fleetCommand(ctx context.Context, turn string) {
	// Commands stay responsive while the separate operation worker drains or copies.
	m := e.s.View().Members[fleetID]
	t := findTurn(m, turn)
	if t == nil || t.Phase == "done" {
		return
	}
	if t.Sender != "@"+e.c.Admin+":"+e.c.ServerName || t.Room != m.Room || t.Thread != "" {
		_ = e.s.Update(func(st *State) error { findTurn(st.Members[fleetID], turn).Phase = "done"; return nil })
		return
	}
	if t.MenuID != "" && !e.resolveMenu(fleetID, turn) {
		return
	}
	t = findTurn(e.s.View().Members[fleetID], turn)
	// A plain reply is only meaningful as the answer to a pending name prompt.
	if t.Kind != "command" {
		e.fleetPromptAnswer(turn)
		return
	}
	f := strings.Fields(t.Text)
	action, value, name := "home", "", ""
	if t.Selection != nil {
		action, value = t.Selection.Action, t.Selection.Value
	} else if len(f) > 0 && f[0] == "!fleet" {
		if len(f) > 1 {
			action = f[1]
		}
		if len(f) > 2 {
			value = f[2]
		}
		if action == "create" || action == "copy" || action == "save-template" || action == "rename" {
			separator := map[string]string{"create": "from", "copy": "to", "save-template": "as", "rename": "to"}[action]
			if len(f) != 5 || f[3] != separator {
				action = "help"
			} else {
				name = f[4]
			}
		} else if strings.HasPrefix(action, "confirm-") {
			if len(f) != 4 {
				action = "help"
			} else {
				name = f[3]
			}
		}
	} else if len(f) > 0 && f[0] != "!menu" {
		action = "help"
	}
	if action == "status" {
		view := e.s.View()
		a := fleetMember(view, value)
		if t.Selection != nil {
			a = view.Members[value]
		}
		if a == nil {
			a = view.Members[value]
		}
		text := "Agent not found."
		if a != nil && a.Managed && !a.Deleted {
			state := "Unknown (inspection failed)"
			_, err := e.docker.endpoint(ctx, a)
			if err == nil {
				state = "Running"
			} else if errors.Is(err, errStopped) {
				state = "Stopped"
			}
			text = agentName(a) + " · " + fleetAvailability(a) + "\nContainers: " + state + fmt.Sprintf("\nPending turns: %d · Replies awaiting delivery: %d", pending(a), len(a.Outbox))
		}
		_ = e.s.Update(func(st *State) error { fleetReply(st, findTurn(st.Members[fleetID], turn), text); return nil })
		return
	}
	_ = e.s.Update(func(st *State) error {
		t := findTurn(st.Members[fleetID], turn)
		switch action {
		case "home", "agents", "templates", "agent", "template", "more":
			fleetMenu(st, t, action, value, e.c.TemplateDir)
			return nil
		case "help":
			fleetReply(st, t, fleetHelp)
			return nil
		case "copy-help", "save-help":
			a := st.Members[value]
			if a == nil || a.Deleted {
				fleetReply(st, t, "Agent is no longer available.")
				return nil
			}
			text := "!fleet copy " + agentName(a) + " to <new-name>"
			if action == "save-help" {
				text = "!fleet save-template " + agentName(a) + " as <template-name>"
			}
			fleetReply(st, t, text+"\nFull copy, including private data and provider credentials. Source work drains before a stopped snapshot.")
			return nil
		case promptCopy, promptTemplate, promptRename:
			a := st.Members[value]
			if a == nil || a.Deleted || !a.Managed {
				fleetReply(st, t, "Agent is no longer available.")
				return nil
			}
			if action == promptCopy {
				fleetStartPrompt(st, t, promptCopy, a.ID, "Copy "+agentName(a)+" → reply with the new agent name.", "!fleet copy "+agentName(a)+" to <new-name>")
			} else if action == promptRename {
				fleetStartPrompt(st, t, promptRename, a.ID, "Rename "+agentName(a)+" → reply with the new agent name.", "!fleet rename "+agentName(a)+" to <new-name>")
			} else {
				fleetStartPrompt(st, t, promptTemplate, a.ID, "Save "+agentName(a)+" as a template → reply with the template name.", "!fleet save-template "+agentName(a)+" as <template-name>")
			}
			return nil
		case "cancel":
			if op := st.FleetOperation; op != nil {
				if op.Stage != "waiting" {
					fleetReply(st, t, "Cannot cancel: this operation has passed the waiting phase. Fix any reported problem and retry.")
					return nil
				}
				if a := st.Members[op.Source]; a != nil {
					a.Draining = false
				}
				st.FleetOperation = nil
			}
			st.FleetConfirmation = nil
			st.FleetPrompt = nil
			fleetReply(st, t, "Pending operation/confirmation cancelled.")
			return nil
		case "retry":
			if st.FleetOperation != nil {
				st.FleetOperation.Error = ""
			}
			fleetReply(st, t, "Operation retry requested.")
			return nil
		case "confirm-delete", "confirm-create", "confirm-copy":
			cf := st.FleetConfirmation
			if cf == nil || cf.Owner != t.Sender || time.Now().After(cf.Expires) || cf.Code != name || cf.Name != value || "confirm-"+cf.Action != action {
				fleetReply(st, t, "Confirmation is invalid or expired; request the operation again.")
				return nil
			}
			if st.FleetOperation != nil {
				fleetReply(st, t, "Another fleet operation is pending.")
				return nil
			}
			if cf.Action == "delete" {
				a := st.Members[cf.Target]
				if a == nil || a.Deleted || agentName(a) != cf.Name {
					fleetReply(st, t, "Target changed; request deletion again.")
					return nil
				}
				startFleetOperation(st, t, "delete", a.ID, "", "", 0)
			} else {
				if fleetMember(*st, cf.Name) != nil {
					fleetReply(st, t, "Name is already in use.")
					return nil
				}
				if cf.Action == "copy" {
					a := st.Members[cf.Source]
					if a == nil || a.Deleted || !a.Managed {
						fleetReply(st, t, "Copy source changed; request the operation again.")
						return nil
					}
				}
				startFleetOperation(st, t, cf.Action, cf.Source, cf.Name, cf.Target, nextFleetPort(*st))
			}
			st.FleetConfirmation = nil
			return nil
		case "force-delete":
			// A force delete is the operator's explicit override of a delete that
			// cannot drain. It is confirmed separately, and it may replace an
			// operation that is stuck waiting on the same broken agent.
			a := fleetMember(*st, value)
			if a == nil {
				a = st.Members[value]
			}
			if a == nil || a.Deleted || !a.Managed || a.Controller {
				fleetReply(st, t, "Managed agent not found. Use !fleet agents.")
				return nil
			}
			cf := &FleetConfirmation{Action: "force-delete", Target: a.ID, Name: agentName(a), Code: randomSecret()[:8], Owner: t.Sender, Expires: time.Now().Add(5 * time.Minute)}
			st.FleetConfirmation = cf
			st.FleetPrompt = nil
			fleetReply(st, t, "Force delete "+cf.Name+"?\nThis stops the agent's containers without draining admitted work, erases its managed directory including files, memories, history and credentials, purges its Matrix room, and deactivates its account. In-flight work cannot be drained and will be stopped; tool effects already made are not undone. Any container that mounts this agent's directory is stopped too. Retain saved templates, other copies and shared images.\nConfirm within five minutes:\n!fleet confirm-force-delete "+cf.Name+" "+cf.Code+"\n!fleet cancel to cancel.")
			return nil
		case "confirm-force-delete":
			cf := st.FleetConfirmation
			if cf == nil || cf.Owner != t.Sender || time.Now().After(cf.Expires) || cf.Code != name || cf.Name != value || cf.Action != "force-delete" {
				fleetReply(st, t, "Confirmation is invalid or expired; request the operation again.")
				return nil
			}
			a := st.Members[cf.Target]
			if a == nil || a.Deleted || agentName(a) != cf.Name {
				fleetReply(st, t, "Target changed; request deletion again.")
				return nil
			}
			// Replace a stuck operation on the same agent; refuse to disturb another.
			if op := st.FleetOperation; op != nil {
				if op.Source != a.ID || op.Action != "delete" {
					fleetReply(st, t, "Another fleet operation is pending. Use !fleet to inspect it, cancel while waiting, or retry after fixing a problem.")
					return nil
				}
				if op.Stage != "waiting" && op.Stage != "stopping" {
					fleetReply(st, t, "The delete has already passed the point where it can be forced. Use !fleet to inspect it.")
					return nil
				}
				if a := st.Members[op.Source]; a != nil {
					a.Draining = false
				}
				st.FleetOperation = nil
			}
			st.FleetConfirmation = nil
			startForcedDelete(st, t, a.ID)
			return nil
		}
		if st.FleetOperation != nil {
			fleetReply(st, t, "Another fleet operation is pending. Use !fleet to inspect it, cancel while waiting, or retry after fixing a problem.")
			return nil
		}
		if action == "create" {
			e.fleetBeginNamed(st, t, "create", name, nil, value)
			return nil
		}
		a := fleetMember(*st, value)
		if t.Selection != nil {
			a = st.Members[value]
		}
		if a == nil || a.Deleted || !a.Managed {
			a = st.Members[value]
		}
		if a == nil || a.Deleted || !a.Managed || a.Controller {
			fleetReply(st, t, "Managed agent not found. Use !fleet agents.")
			return nil
		}
		switch action {
		case "delete":
			cf := &FleetConfirmation{Action: "delete", Target: a.ID, Name: agentName(a), Code: randomSecret()[:8], Owner: t.Sender, Expires: time.Now().Add(5 * time.Minute)}
			st.FleetConfirmation = cf
			st.FleetPrompt = nil
			fleetReply(st, t, "Permanently delete "+cf.Name+"?\nErase this agent's containers and managed directory, including files, memories, history and credentials, and purge its Matrix room with all its messages. Drain admitted work first; never force-kill. Retain saved templates, other copies and shared images. Remove from Fleet menus and attempt Matrix account deactivation.\nConfirm within five minutes:\n!fleet confirm-delete "+cf.Name+" "+cf.Code+"\n!fleet cancel to cancel.")
		case "enable":
			a.Paused = false
			fleetReply(st, t, agentName(a)+" is available for message-triggered wake.")
		case "start", "shutdown", "pause":
			if action == "start" && a.Paused {
				fleetReply(st, t, "Enable this paused agent before starting it.")
				return nil
			}
			startFleetOperation(st, t, action, a.ID, "", "", 0)
		case "copy", "save-template", "rename":
			e.fleetBeginNamed(st, t, action, "", a, name)
		default:
			fleetReply(st, t, fleetHelp)
		}
		return nil
	})
}

func fleetName(name string) bool {
	return len(name) <= 40 && localpart.MatchString(name) && name != fleetID && name != "." && name != ".."
}

// fleetStartPrompt asks the operator for a destination name. A pending confirmation
// blocks it, so only one interaction is ever outstanding.
func fleetStartPrompt(st *State, t *Turn, action, source, headline, hint string) {
	if st.FleetConfirmation != nil {
		fleetReply(st, t, "Finish or cancel the pending confirmation first (!fleet cancel).")
		return
	}
	st.FleetPrompt = &FleetPrompt{Action: action, Source: source, Owner: t.Sender, Expires: time.Now().Add(5 * time.Minute)}
	fleetReply(st, t, headline+"\n(or: "+hint+")\nCancels with !fleet cancel · expires in 5 minutes")
}

// fleetPromptAnswer consumes the operator's plain reply to a pending name prompt.
func (e *Engine) fleetPromptAnswer(turn string) {
	_ = e.s.Update(func(st *State) error {
		t := findTurn(st.Members[fleetID], turn)
		p := st.FleetPrompt
		if p == nil {
			t.Phase = "done"
			return nil
		}
		if p.Owner != t.Sender || time.Now().After(p.Expires) {
			st.FleetPrompt = nil
			fleetReply(st, t, "That name prompt expired. Tap the action again.")
			return nil
		}
		name := strings.TrimSpace(t.Text)
		switch p.Action {
		case promptCopy:
			a := st.Members[p.Source]
			if a == nil || a.Deleted || !a.Managed {
				st.FleetPrompt = nil
				fleetReply(st, t, "Copy source is no longer available.")
				return nil
			}
			e.fleetBeginNamed(st, t, "copy", "", a, name)
		case promptTemplate, promptRename:
			a := st.Members[p.Source]
			if a == nil || a.Deleted || !a.Managed {
				st.FleetPrompt = nil
				fleetReply(st, t, "Source agent is no longer available.")
				return nil
			}
			action := "save-template"
			if p.Action == promptRename {
				action = "rename"
			}
			e.fleetBeginNamed(st, t, action, "", a, name)
		case promptCreate:
			e.fleetBeginNamed(st, t, "create", p.Source, nil, name)
		default:
			st.FleetPrompt = nil
			fleetReply(st, t, "That prompt is no longer available.")
		}
		return nil
	})
}

// fleetBeginNamed validates a destination name and starts the named operation. The
// typed command and the name prompt share it, so both behave identically. A
// rejected name leaves any pending prompt alive so the operator can retry.
func (e *Engine) fleetBeginNamed(st *State, t *Turn, action, template string, a *Member, name string) {
	if st.FleetOperation != nil || st.FleetConfirmation != nil {
		fleetReply(st, t, "Finish or cancel the pending fleet operation or confirmation first (!fleet cancel).")
		return
	}
	if !fleetName(name) {
		fleetReply(st, t, "Not a valid agent name. Use up to 40 lowercase letters, digits, dots, dashes or underscores. Reply again, or !fleet cancel.")
		return
	}
	switch action {
	case "rename":
		for id, other := range st.Members {
			if id != a.ID && (agentName(other) == name || id == name) {
				fleetReply(st, t, "Name is already in use or belongs to a retained identity. Choose another name.")
				return
			}
		}
		st.FleetPrompt = nil
		st.FleetOperation = &FleetOperation{ID: t.ID, Action: "rename", Source: a.ID, Name: name, Stage: "renaming"}
		fleetReply(st, t, "Rename queued: "+agentName(a)+" → "+name+". Only the bot display name and dedicated room name change; the agent keeps running.")
	case "create":
		if fleetMember(*st, name) != nil {
			fleetReply(st, t, "Agent name is already in use.")
			return
		}
		tpl, err := readFleetTemplate(e.c.TemplateDir, template)
		if err != nil {
			fleetReply(st, t, err.Error())
			return
		}
		id := name + "-" + randomSecret()[:12]
		for _, old := range st.Members {
			if old.Deleted && agentName(old) == name {
				cf := &FleetConfirmation{Action: "create", Name: name, Source: tpl.Name, Target: id, Code: randomSecret()[:8], Owner: t.Sender, Expires: time.Now().Add(5 * time.Minute)}
				st.FleetConfirmation = cf
				st.FleetPrompt = nil
				fleetReply(st, t, "Warning: this name belonged to a deleted agent. Its old Matrix rooms/history remain. The replacement gets a fresh identity and room. Confirm within five minutes:\n!fleet confirm-create "+name+" "+cf.Code)
				return
			}
		}
		st.FleetPrompt = nil
		startFleetOperation(st, t, "create", tpl.Name, name, id, nextFleetPort(*st))
	case "copy":
		if fleetMember(*st, name) != nil {
			fleetReply(st, t, "Agent name is already in use.")
			return
		}
		for _, old := range st.Members {
			if old.Deleted && agentName(old) == name {
				cf := &FleetConfirmation{Action: "copy", Name: name, Source: a.ID, Target: name + "-" + randomSecret()[:12], Code: randomSecret()[:8], Owner: t.Sender, Expires: time.Now().Add(5 * time.Minute)}
				st.FleetConfirmation = cf
				st.FleetPrompt = nil
				fleetReply(st, t, "Warning: this name belonged to a deleted agent. The copy gets a fresh identity and room; the deleted agent's own room was erased with it. Confirm within five minutes:\n!fleet confirm-copy "+name+" "+cf.Code)
				return
			}
		}
		st.FleetPrompt = nil
		startFleetOperation(st, t, "copy", a.ID, name, name+"-"+randomSecret()[:12], nextFleetPort(*st))
	case "save-template":
		if _, err := os.Lstat(filepath.Join(e.c.TemplateDir, name)); !os.IsNotExist(err) {
			fleetReply(st, t, "Template destination already exists or is unavailable; templates are never overwritten.")
			return
		}
		st.FleetPrompt = nil
		startFleetOperation(st, t, "save-template", a.ID, name, name+"-"+randomSecret()[:12], nextFleetPort(*st))
	}
}

func nextFleetPort(st State) int {
	used := map[int]bool{}
	for _, m := range st.Members {
		if !m.Deleted {
			used[m.Port] = true
		}
	}
	for port := 18642; port < 65536; port++ {
		if !used[port] {
			return port
		}
	}
	return 0
}

func startFleetOperation(st *State, t *Turn, action, source, name, target string, port int) {
	op := &FleetOperation{ID: t.ID, Action: action, Source: source, Name: name, Target: target, Port: port, Stage: "waiting"}
	if action != "create" {
		a := st.Members[source]
		op.WasPaused = a.Paused
		a.Draining = true
	}
	st.FleetOperation = op
	fleetReply(st, t, "Fleet operation queued: "+action+". New work is blocked while admitted work drains. Use !fleet for progress; !fleet cancel works until draining completes.")
}

// startForcedDelete starts a delete that the operator explicitly forced, so it does not wait
// for admitted work to drain.
func startForcedDelete(st *State, t *Turn, source string) {
	startFleetOperation(st, t, "delete", source, "", "", 0)
	if st.FleetOperation != nil {
		st.FleetOperation.Forced = true
	}
}

func (e *Engine) fleetStep(ctx context.Context) {
	e.fleetMu.Lock()
	defer e.fleetMu.Unlock()
	view := e.s.View()
	op := view.FleetOperation
	if op == nil || op.Error != "" {
		return
	}
	if err := e.runFleetOperation(ctx, op); err != nil && ctx.Err() == nil {
		_ = e.s.Update(func(st *State) error {
			if current := st.FleetOperation; current != nil && current.ID == op.ID {
				current.Error = err.Error()
				m := st.Members[fleetID]
				text := "Fleet operation held: " + err.Error() + " Fix the problem locally, then !fleet retry."
				if op.Action != "rename" {
					text += " Source remains guarded; no force-stop or rollback of tool effects."
				}
				addNotice(m, op.ID+"-error-"+stableID(err.Error())[:8], m.Room, "", text)
			}
			return nil
		})
	}
}

func (e *Engine) fleetStage(op *FleetOperation, stage string) error {
	return e.s.Update(func(st *State) error {
		if st.FleetOperation == nil || st.FleetOperation.ID != op.ID {
			return errors.New("fleet operation was cancelled")
		}
		st.FleetOperation.Stage = stage
		if stage == "deleting" {
			a := st.Members[op.Source]
			a.Deleted = true
			a.Enabled = false
			a.Paused = true
		}
		return nil
	})
}

func (e *Engine) runFleetOperation(ctx context.Context, op *FleetOperation) error {
	if !validFleetOperation(op, e.s.View()) {
		return errors.New("invalid fleet operation; refusing unsafe recovery")
	}
	if op.Action == "rename" {
		a := e.s.View().Members[op.Source]
		bot := e.mx.cred("agent_" + a.ID)
		if err := e.mx.nameBot(ctx, bot, op.Name); err != nil {
			return errors.New("bot display-name update failed; retry to complete the rename")
		}
		if err := api(ctx, "PUT", e.mx.Base, roomPath(a.Room)+"/state/m.room.name/", bot.Token, map[string]string{"name": op.Name}, nil, nil); err != nil {
			return errors.New("room-name update failed; bot name may already have changed; retry to complete the rename")
		}
		// Record the name the rename wrote, so the session-name writer knows the
		// visible name it last set and does not write over the rename.
		_ = e.s.Update(func(st *State) error {
			if member := st.Members[op.Source]; member != nil {
				if tracked := member.Rooms[member.Room]; tracked != nil {
					tracked.RoomName = op.Name
				}
			}
			return nil
		})
		// A shared room's name carries the agent's name as its prefix, so it moves
		// with the rename. The session names are already known, so this needs no
		// Hermes call.
		e.renameSharedRooms(ctx, op.Source, op.Name)
		return e.finishFleet(op, "")
	}
	var a *Member
	if op.Action != "create" {
		a = e.s.View().Members[op.Source]
		if a == nil || !a.Managed {
			return errors.New("managed source is unavailable")
		}
	}
	if op.Stage == "waiting" {
		if a != nil {
			rt := e.runtime(a.ID)
			rt.lifecycle.Lock()
			defer rt.lifecycle.Unlock()
			rt.mu.Lock()
			busy := len(rt.busy) > 0 || len(rt.controls) > 0
			rt.mu.Unlock()
			if err := e.docker.validateManaged(ctx, e.c, a); err != nil {
				return err
			}
			if err := e.docker.checkProject(ctx, a); err != nil {
				return err
			}
			endpoint, err := e.docker.endpoint(ctx, a)
			if err != nil && !errors.Is(err, errStopped) {
				return err
			}
			// The agent's own state is checked before its admitted work, so a broken
			// agent is reported as broken instead of waiting for work that can
			// never be drained. A merely busy agent is a normal wait.
			if err == nil && op.Action != "start" && !op.Forced {
				idle, reason := e.fleetInactive(ctx, a, endpoint)
				if !idle && errors.Is(reason, errAgentUnreachable) && op.Action == "delete" {
					return e.fleetHoldDelete(ctx, op, a, reason)
				}
				if !idle {
					return nil
				}
			}
			if busy || pending(a) > 0 {
				if op.Forced {
					// A forced delete does not wait for admitted work to finish.
					return e.fleetStage(op, "stopping")
				}
				return nil
			}
			if err = e.s.Update(func(st *State) error {
				if st.FleetOperation == nil || st.FleetOperation.ID != op.ID {
					return errors.New("fleet operation was cancelled")
				}
				st.FleetOperation.WasRunning = endpoint != ""
				st.FleetOperation.Stage = "stopping"
				return nil
			}); err != nil {
				return err
			}
		} else {
			return e.fleetStage(op, "copying")
		}
		return nil
	}
	if op.Stage == "stopping" {
		if op.Action == "start" {
			if _, _, err := e.ensure(ctx, a.ID, a.Room); err != nil {
				return err
			}
			return e.finishFleet(op, "")
		}
		if err := e.docker.validateManaged(ctx, e.c, a); err != nil {
			return err
		}
		if err := e.docker.checkProject(ctx, a); err != nil {
			return err
		}
		if endpoint, err := e.docker.endpoint(ctx, a); err == nil {
			if !op.Forced {
				idle, reason := e.fleetInactive(ctx, a, endpoint)
				if !idle {
					return errors.New("agent activity changed; refusing shutdown: " + reason.Error())
				}
			}
		} else if !errors.Is(err, errStopped) {
			return err
		}
		stopCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		// A deleted agent loses its outbox and its Matrix account, so it cannot be told.
		if op.Action != "delete" {
			e.lifecycleNote(a.ID, "stop", "Agent "+agentName(a)+" is shutting down (fleet "+op.Action+").")
		}
		if _, err := e.docker.compose(stopCtx, a, "down", "--remove-orphans", "--timeout", "60"); err != nil {
			return err
		}
		if op.Forced {
			// The managed project is down, but an unmanaged container may still
			// mount the agent's directory. Stop it before the storage is erased.
			if err := e.docker.stopMountHolders(stopCtx, a.Dir); err != nil {
				return err
			}
		}
		switch op.Action {
		case "pause", "shutdown":
			return e.finishFleet(op, "")
		case "delete":
			return e.fleetStage(op, "deleting")
		default:
			return e.fleetStage(op, "copying")
		}
	}
	if op.Stage == "deleting" {
		e.mediaMu.Lock()
		defer e.mediaMu.Unlock()
		if err := e.docker.fleetProjectEmpty(ctx, a); err != nil {
			return err
		}
		// An unmanaged container may still mount the agent's directory. Erasing it
		// underneath such a container leaves a live process writing into a deleted
		// path, so stop it first and refuse to erase while one remains.
		holders, err := e.docker.mountHolders(ctx, a.Dir)
		if err != nil {
			return err
		}
		if len(holders) > 0 {
			if op.Forced {
				if err := e.docker.stopMountHolders(ctx, a.Dir); err != nil {
					return err
				}
				holders, err = e.docker.mountHolders(ctx, a.Dir)
				if err != nil {
					return err
				}
			}
			if len(holders) > 0 {
				return errors.New("a container outside this agent's Compose project still mounts its directory; stop it before deleting")
			}
		}
		if err := deleteFleetTree(e.c.FleetDir, a.Dir); err != nil {
			return err
		}
		// The room goes before the bot is deactivated, so a failed purge leaves the
		// account intact and the retry stays clean. Only the agent's own room is
		// purged; operator-created rooms and the Fleet room are untouched.
		if a.Room != "" {
			if err := e.mx.purgeRoom(ctx, a.Room); err != nil {
				return errors.New("Matrix room purge failed: " + err.Error() + "; retry to erase the deleted agent's room")
			}
		}
		warning := ""
		if err := e.mx.deactivateAgent(ctx, a.ID); err != nil {
			if !errors.Is(err, errDeactivationUnavailable) {
				return err
			}
			warning = " Matrix account deactivation could not be confirmed; bridge dispatch is permanently disabled."
		}
		return e.finishFleet(op, warning)
	}
	if op.Stage == "copying" {
		e.mediaMu.Lock()
		defer e.mediaMu.Unlock()
		if a != nil {
			if err := e.docker.fleetProjectEmpty(ctx, a); err != nil {
				return err
			}
		}
		if err := e.publishFleetCopy(ctx, op, a); err != nil {
			return err
		}
		return e.fleetStage(op, "provisioning")
	}
	if op.Stage == "provisioning" {
		if op.Action != "save-template" {
			if err := e.mx.provision(ctx, e.s, op.Target); err != nil {
				return errors.New("new agent Matrix provisioning failed; retry preserves its identity and files")
			}
			e.startMember(ctx, op.Target)
		}
		if a != nil && op.WasRunning && !op.WasPaused {
			if _, _, err := e.ensure(ctx, a.ID, a.Room); err != nil {
				return errors.New("copy saved but source restart failed; retry to restore its availability")
			}
		}
		return e.finishFleet(op, "")
	}
	return errors.New("unknown fleet operation phase; refusing unsafe recovery")
}

func validFleetOperation(op *FleetOperation, st State) bool {
	if op.ID == "" || (op.Stage == "renaming" && op.Action != "rename") {
		return false
	}
	switch op.Stage {
	case "waiting", "stopping", "copying", "provisioning", "deleting", "renaming":
	default:
		return false
	}
	switch op.Action {
	case "rename":
		if op.Stage != "renaming" || !fleetName(op.Name) {
			return false
		}
	case "create":
		return (op.Stage == "waiting" || op.Stage == "copying" || op.Stage == "provisioning") && fleetName(op.Source) && fleetName(op.Name) && localpart.MatchString(op.Target) && op.Port >= 1024 && op.Port <= 65535
	case "copy", "save-template":
		if op.Stage == "deleting" || !fleetName(op.Name) || !localpart.MatchString(op.Target) || op.Port < 1024 || op.Port > 65535 {
			return false
		}
	case "start", "shutdown", "pause", "delete":
		if op.Stage != "waiting" && op.Stage != "stopping" && !(op.Action == "delete" && op.Stage == "deleting") {
			return false
		}
	default:
		return false
	}
	a := st.Members[op.Source]
	return a != nil && a.Managed && !a.Controller && (!a.Deleted || (op.Action == "delete" && op.Stage == "deleting"))
}

// fleetInactive reports whether the agent is provably idle through its authenticated API.
// It distinguishes "cannot tell" from "busy": the caller must not treat an unreachable API
// as a quiet agent, because that is how a broken agent turns a delete into an endless wait.
func (e *Engine) fleetInactive(ctx context.Context, m *Member, endpoint string) (bool, error) {
	var h detailedHealth
	if err := api(ctx, "GET", endpoint, "/health/detailed", e.mx.key(m.ID), nil, &h, nil); err != nil {
		return false, fmt.Errorf("%w: %w", errAgentUnreachable, err)
	}
	b := h.Readiness.Checks.Background
	idle := h.Readiness.Status == "ok" && h.Busy != nil && !*h.Busy && h.Drainable != nil && *h.Drainable && h.Active != nil && *h.Active == 0 && b.Runs != nil && *b.Runs == 0 && b.Processes != nil && *b.Processes == 0 && b.Delegates != nil && *b.Delegates == 0
	if !idle {
		return false, errAgentBusy
	}
	return true, nil
}

// fleetHoldDelete reports a delete that cannot drain through the agent's API and offers the
// operator the force. The offer is a separate, explicitly confirmed step: the operator says
// yes to erasing an agent whose work cannot be drained.
func (e *Engine) fleetHoldDelete(ctx context.Context, op *FleetOperation, a *Member, reason error) error {
	_ = e.s.Update(func(st *State) error {
		current := st.FleetOperation
		if current == nil || current.ID != op.ID {
			return nil
		}
		current.Error = reason.Error()
		m := st.Members[fleetID]
		text := "Fleet operation held: " + reason.Error() + "."
		if pending(a) > 0 {
			text += " This agent has admitted work that only a working API can drain, so !discard cannot release it either."
		}
		text += " Fix the problem locally, then !fleet retry."
		if errors.Is(reason, errAgentUnreachable) {
			text += "\nOr delete it anyway with !fleet force-delete " + agentName(a) + " — that stops its containers without draining, erases its directory, and deactivates its Matrix account. In-flight work cannot be drained and will be stopped."
		}
		addNotice(m, op.ID+"-error-"+stableID(reason.Error())[:8], m.Room, "", text)
		return nil
	})
	return reason
}

// finishFleet completes an operation and clears the durable record.
func (e *Engine) finishFleet(op *FleetOperation, warning string) error {
	return e.s.Update(func(st *State) error {
		if st.FleetOperation == nil || st.FleetOperation.ID != op.ID {
			return errors.New("fleet operation changed")
		}
		if a := st.Members[op.Source]; a != nil && op.Action != "create" {
			if op.Action == "rename" {
				a.Name = op.Name
			}
			a.Draining = false
			if op.Action == "start" || ((op.Action == "copy" || op.Action == "save-template") && op.WasRunning) {
				a.LastActivity = time.Now().UTC()
			}
			if op.Action == "pause" {
				a.Paused = true
			}
			if op.Action == "delete" {
				a.Deleted, a.Enabled, a.Paused = true, false, true
				a.Turns = nil
				a.Outbox = nil
				a.Menus = nil
				a.Sessions = map[string]map[string]*Session{}
			}
		}
		m := st.Members[fleetID]
		name := op.Name
		if name == "" {
			if a := st.Members[op.Source]; a != nil {
				name = agentName(a)
			}
		}
		addNotice(m, op.ID+"-complete", m.Room, "", "Fleet operation complete: "+op.Action+" "+name+warning)
		st.FleetOperation = nil
		return nil
	})
}

// Tuwunel 1.9.2 supports the administrator route with erase=false (history retained).
func (mx *Matrix) deactivateAgent(ctx context.Context, id string) error {
	mx.mu.Lock()
	if mx.secrets.Retired == nil {
		mx.secrets.Retired = map[string]bool{}
	}
	mx.secrets.Retired["agent_"+id] = true
	err := mx.saveSecrets()
	mx.mu.Unlock()
	if err != nil {
		return err
	}
	admin := mx.cred(mx.c.Admin)
	err = api(ctx, "POST", mx.Base, "/_synapse/admin/v1/deactivate/"+url.PathEscape("@agent_"+id+":"+mx.c.ServerName), admin.Token, map[string]any{"erase": false}, nil, nil)
	mx.mu.Lock()
	defer mx.mu.Unlock()
	delete(mx.secrets.APIKeys, id)
	delete(mx.secrets.Accounts, "agent_"+id)
	if saveErr := mx.saveSecrets(); saveErr != nil {
		return saveErr
	}
	if err != nil {
		return errDeactivationUnavailable
	}
	return nil
}
