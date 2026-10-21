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
)

var errDeactivationUnavailable = errors.New("Matrix account deactivation unconfirmed")

type FleetConfirmation struct {
	Action, Target, Name, Source, Code, Owner string
	Expires                                   time.Time
}

// One durable operation at a time keeps ownership, draining and recovery unambiguous.
type FleetOperation struct {
	ID, Action, Source, Name, Target, Stage, Error string
	WasRunning, WasPaused                          bool
	Port                                           int
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
	for _, dir := range c.Dirs {
		if dirs[dir] != "" {
			continue
		}
		deleted := false
		for _, m := range view.Members {
			if m.Deleted && m.Dir == dir {
				deleted = true
			}
		}
		if !deleted {
			dirs[dir] = filepath.Base(dir)
		}
	}
	projects := map[string]bool{}
	ports := map[int]bool{}
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
		}
		m, err := d.discover(ctx, dir, o)
		if err != nil {
			return fmt.Errorf("agent %s: %w", filepath.Base(dir), err)
		}
		m.ID, m.Name, m.Managed = id, filepath.Base(dir), true
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
		host := "0.0.0.0"
		if m.Network == "host" {
			host = "127.0.0.1"
		}
		key, err := configureEnv(filepath.Join(m.Home, ".env"), host, m.Port)
		if err != nil {
			return err
		}
		if err = mx.rememberKey(id, key); err != nil {
			return err
		}
		if err = s.Update(func(st *State) error {
			if prev := st.Members[id]; prev != nil {
				prev.Managed, prev.Name, prev.Enabled = true, m.Name, true
				prev.Dir, prev.Home, prev.Project, prev.Service, prev.Port, prev.Network, prev.Files = m.Dir, m.Home, m.Project, m.Service, m.Port, m.Network, m.Files
			} else {
				st.Members[id] = m
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
!fleet delete <agent>
!fleet cancel — cancel a waiting operation, confirmation or name prompt
!fleet retry — retry a held operation after fixing the reported problem

Copy agent, Save as template and the template list ask for the new name: reply with
just the name. An invalid name keeps the prompt alive; !fleet cancel ends it.

Shutdown allows the next message to wake the agent. Pause drains work and shuts down, preventing wake until enabled. Copies and agents created from templates start ready to receive work. Delete permanently erases agent files, not Matrix history, templates or other copies. Templates can contain private data and provider credentials. No secrets should be pasted into this room.`

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
	case "agent":
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
			{Label: "Start", Action: "start", Value: value},
			{Label: "Shutdown", Action: "shutdown", Value: value}, availability,
			{Label: "Copy agent", Action: promptCopy, Value: value},
			{Label: "Save as template", Action: promptTemplate, Value: value},
			{Label: "Delete", Action: "delete", Value: value},
			{Label: "Back", Action: "home"},
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
		if action == "create" || action == "copy" || action == "save-template" {
			separator := map[string]string{"create": "from", "copy": "to", "save-template": "as"}[action]
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
		a := view.Members[value]
		if a == nil {
			a = fleetMember(view, value)
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
		case "home", "agents", "templates", "agent", "template":
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
		case promptCopy, promptTemplate:
			a := st.Members[value]
			if a == nil || a.Deleted || !a.Managed {
				fleetReply(st, t, "Agent is no longer available.")
				return nil
			}
			if action == promptCopy {
				fleetStartPrompt(st, t, promptCopy, a.ID, "Copy "+agentName(a)+" → reply with the new agent name.", "!fleet copy "+agentName(a)+" to <new-name>")
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
		}
		if st.FleetOperation != nil {
			fleetReply(st, t, "Another fleet operation is pending. Use !fleet to inspect it, cancel while waiting, or retry after fixing a problem.")
			return nil
		}
		if action == "create" {
			e.fleetBeginNamed(st, t, "create", name, nil, value)
			return nil
		}
		a := st.Members[value]
		if a == nil || a.Deleted || !a.Managed {
			a = fleetMember(*st, value)
		}
		if a == nil {
			fleetReply(st, t, "Managed agent not found. Use !fleet agents.")
			return nil
		}
		switch action {
		case "delete":
			cf := &FleetConfirmation{Action: "delete", Target: a.ID, Name: agentName(a), Code: randomSecret()[:8], Owner: t.Sender, Expires: time.Now().Add(5 * time.Minute)}
			st.FleetConfirmation = cf
			st.FleetPrompt = nil
			fleetReply(st, t, "Permanently delete "+cf.Name+"?\nErase this agent's containers and managed directory, including files, memories, history and credentials. Drain admitted work first; never force-kill. Retain Matrix rooms/messages, saved templates, other copies and shared images. Remove from Fleet menus and attempt Matrix account deactivation.\nConfirm within five minutes:\n!fleet confirm-delete "+cf.Name+" "+cf.Code+"\n!fleet cancel to cancel.")
		case "enable":
			a.Paused = false
			fleetReply(st, t, agentName(a)+" is available for message-triggered wake.")
		case "start", "shutdown", "pause":
			if action == "start" && a.Paused {
				fleetReply(st, t, "Enable this paused agent before starting it.")
				return nil
			}
			startFleetOperation(st, t, action, a.ID, "", "", 0)
		case "copy", "save-template":
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
		case promptTemplate:
			a := st.Members[p.Source]
			if a == nil || a.Deleted || !a.Managed {
				st.FleetPrompt = nil
				fleetReply(st, t, "Source agent is no longer available.")
				return nil
			}
			e.fleetBeginNamed(st, t, "save-template", "", a, name)
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
	if !fleetName(name) {
		fleetReply(st, t, "Not a valid agent name. Use up to 40 lowercase letters, digits, dots, dashes or underscores. Reply again, or !fleet cancel.")
		return
	}
	switch action {
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
				fleetReply(st, t, "Warning: this name has retained Matrix history. The copy gets a fresh identity and room. Confirm within five minutes:\n!fleet confirm-copy "+name+" "+cf.Code)
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
				addNotice(m, op.ID+"-error-"+stableID(err.Error())[:8], m.Room, "", "Fleet operation held: "+err.Error()+" Fix the problem locally, then !fleet retry. Source remains guarded; no force-stop or rollback of tool effects.")
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
			if busy || pending(a) > 0 {
				return nil
			}
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
			if err == nil && op.Action != "start" {
				if !e.fleetInactive(ctx, a, endpoint) {
					return nil
				}
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
			if !e.fleetInactive(ctx, a, endpoint) {
				return errors.New("agent activity changed; refusing shutdown")
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
		if err := deleteFleetTree(e.c.FleetDir, a.Dir); err != nil {
			return err
		}
		warning := ""
		if err := e.mx.deactivateAgent(ctx, a.ID); err != nil {
			if !errors.Is(err, errDeactivationUnavailable) {
				return err
			}
			warning = " Matrix account deactivation could not be confirmed; bridge dispatch is permanently disabled. Rooms and messages remain."
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
	if op.ID == "" {
		return false
	}
	switch op.Stage {
	case "waiting", "stopping", "copying", "provisioning", "deleting":
	default:
		return false
	}
	switch op.Action {
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

func (e *Engine) fleetInactive(ctx context.Context, m *Member, endpoint string) bool {
	var h detailedHealth
	if api(ctx, "GET", endpoint, "/health/detailed", e.mx.key(m.ID), nil, &h, nil) != nil {
		return false
	}
	b := h.Readiness.Checks.Background
	return h.Readiness.Status == "ok" && h.Busy != nil && !*h.Busy && h.Drainable != nil && *h.Drainable && h.Active != nil && *h.Active == 0 && b.Runs != nil && *b.Runs == 0 && b.Processes != nil && *b.Processes == 0 && b.Delegates != nil && *b.Delegates == 0
}

func (e *Engine) finishFleet(op *FleetOperation, warning string) error {
	return e.s.Update(func(st *State) error {
		if st.FleetOperation == nil || st.FleetOperation.ID != op.ID {
			return errors.New("fleet operation changed")
		}
		if a := st.Members[op.Source]; a != nil && op.Action != "create" {
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
