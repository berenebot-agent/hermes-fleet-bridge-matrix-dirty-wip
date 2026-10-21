package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// The Fleet room and every agent room carry one room widget: an HTML panel that renders the
// room's open menu as buttons. The reaction menu stays exactly as it is, so clients without
// widget support (Element X, other apps) keep working unchanged.
//
// The widget talks to the bridge over two endpoints instead of the Matrix Widget API: that
// keeps Element's capability prompts and the matrix-widget-api bundle out of the picture, and
// leaves the bridge acting on the menu exactly as it does for a reaction tap.
//
// The room token lives in the widget URL fragment, so it never reaches a proxy log, and it is
// readable only by room members because the widget URL sits in a room state event. A press is
// attributed to the clicker: the panel sends the user's own access token, the bridge verifies
// it against the homeserver, and the open menu's owner still has to match.

const widgetStateKey = "hermes-fleet-menu"
const widgetName = "Hermes Fleet"
const widgetCSP = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"

const widgetPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Hermes Fleet</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body { margin: 0; padding: 10px; background: #1b1f24; color: #f2f5f7;
         font: 14px/1.45 -apple-system, "Segoe UI", Roboto, "Helvetica Neue", sans-serif; }
  header { display: flex; align-items: center; gap: 9px; padding: 2px 2px 10px; }
  .avatar { width: 26px; height: 26px; border-radius: 8px; display: grid; place-items: center;
            background: #0b7a5c; color: #eafff7; font-size: 12px; font-weight: 700; }
  .name { font-weight: 600; font-size: 13px; }
  .sub { color: #9aa4ae; font-size: 11px; }
  .title { color: #9aa4ae; font-size: 12px; padding: 0 2px 8px; white-space: pre-line; }
  .list { display: flex; flex-direction: column; gap: 6px; }
  button { display: flex; align-items: center; gap: 9px; width: 100%; padding: 9px 11px;
           border-radius: 8px; border: 1px solid #2f353d; background: #22262c; color: #f2f5f7;
           font: inherit; text-align: left; cursor: pointer; }
  button:hover:not(:disabled) { background: #2a3038; }
  button:disabled { opacity: .5; cursor: default; }
  .key { width: 18px; text-align: center; }
  .empty { color: #9aa4ae; text-align: center; padding: 22px 6px; font-size: 13px; }
  .empty b { color: #f2f5f7; }
  .err { color: #ffb4b4; text-align: center; padding: 22px 6px; font-size: 13px; }
  footer { display: flex; justify-content: space-between; gap: 8px; color: #9aa4ae;
           font-size: 11px; padding: 10px 2px 2px; }
</style>
</head>
<body>
<header><span class="avatar">HF</span><div><div class="name">Hermes Fleet</div><div class="sub">Room menu</div></div></header>
<div id="menu" class="empty">Loading…</div>
<footer><span id="who"></span><span>!menu opens fresh controls</span></footer>
<script>
const token = decodeURIComponent((location.hash.split("k=")[1] || "").trim());
let access = "";
try { access = localStorage.getItem("mx_access_token") || ""; } catch (e) { access = ""; }
const menu = document.getElementById("menu");
const who = document.getElementById("who");
if (!access) { who.textContent = "sign in through Element"; }

function show(html, cls) { menu.className = cls; menu.innerHTML = html; }

function render(data) {
  if (!data || !data.options || !data.options.length) {
    show("No open menu.<br><b>Send !menu</b> in this room.", "empty");
    return;
  }
  menu.className = "list";
  menu.innerHTML = "";
  const title = document.createElement("div");
  title.className = "title";
  title.textContent = data.title || "";
  menu.appendChild(title);
  for (const option of data.options) {
    const button = document.createElement("button");
    const key = document.createElement("span");
    key.className = "key";
    key.textContent = option.key || "";
    const label = document.createElement("span");
    label.textContent = option.label || "";
    button.appendChild(key);
    button.appendChild(label);
    button.onclick = async () => {
      button.disabled = true;
      try {
        const reply = await fetch("/widget/select", {
          method: "POST",
          headers: { "Content-Type": "application/json", "X-Fleet-Token": token, "Authorization": "Bearer " + access },
          body: JSON.stringify({ menu: data.menu, key: option.key })
        });
        if (!reply.ok) {
          const body = await reply.text();
          show(body || "This menu is closed.", "err");
          return;
        }
        await poll();
      } catch (e) {
        show("The bridge is unreachable.", "err");
      } finally {
        button.disabled = false;
      }
    };
    menu.appendChild(button);
  }
}

async function poll() {
  try {
    const reply = await fetch("/widget/menu", { headers: { "X-Fleet-Token": token }, cache: "no-store" });
    if (!reply.ok) { show("This widget is not authorised for this room.", "err"); return; }
    render(await reply.json());
  } catch (e) {
    show("The bridge is unreachable.", "err");
  }
  setTimeout(poll, 2000);
}
poll();
</script>
</body>
</html>
`

// widgetOption is one button in the panel: the same key and label the reaction menu offers.
type widgetOption struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// widgetMenuReply is the panel's view of the room's open menu.
type widgetMenuReply struct {
	Menu    string         `json:"menu"`
	Title   string         `json:"title"`
	Options []widgetOption `json:"options"`
}

// widgetRoutes serves the room widget panel and its endpoints, and passes everything else on.
func (e *Engine) widgetRoutes(ready *atomic.Bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/widget/") {
			next.ServeHTTP(w, r)
			return
		}
		if !ready.Load() {
			http.Error(w, "Initializing private chat; retry shortly", http.StatusServiceUnavailable)
			return
		}
		e.widgetHandler(w, r)
	})
}

// widgetHandler serves the panel and its two endpoints.
func (e *Engine) widgetHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The panel is framed by Element, which is served from the same origin.
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	switch r.URL.Path {
	case "/widget/fleet":
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", widgetCSP)
		w.Write([]byte(widgetPage))
	case "/widget/menu":
		if r.Method != "GET" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		e.widgetMenu(w, r)
	case "/widget/select":
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		e.widgetSelect(w, r)
	default:
		http.NotFound(w, r)
	}
}

// widgetRoom finds the room a widget token belongs to. The token is stored with the room, so
// it also names the agent that serves it.
func (e *Engine) widgetRoom(token string) (string, string, bool) {
	if token == "" {
		return "", "", false
	}
	view := e.s.View()
	for id, m := range view.Members {
		if m == nil || m.Deleted {
			continue
		}
		for room, r := range m.Rooms {
			if r == nil || r.WidgetToken == "" {
				continue
			}
			if subtle.ConstantTimeCompare([]byte(r.WidgetToken), []byte(token)) == 1 {
				return id, room, true
			}
		}
	}
	return "", "", false
}

// widgetMenu answers with the room's open menu, or with nothing when the room has none.
func (e *Engine) widgetMenu(w http.ResponseWriter, r *http.Request) {
	id, room, ok := e.widgetRoom(r.Header.Get("X-Fleet-Token"))
	if !ok {
		http.Error(w, "unknown widget", http.StatusNotFound)
		return
	}
	reply := widgetMenuReply{}
	m := e.s.View().Members[id]
	if key, menu := currentMenu(m, room); menu != nil {
		reply.Menu = key
		reply.Title = menu.Title
		for _, option := range menu.Options {
			reply.Options = append(reply.Options, widgetOption{Key: option.Key, Label: option.Label})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(jsonBytes(reply))
}

// widgetSelect turns a press into the same queued turn a reaction tap produces, so both
// surfaces run through one code path.
func (e *Engine) widgetSelect(w http.ResponseWriter, r *http.Request) {
	id, room, ok := e.widgetRoom(r.Header.Get("X-Fleet-Token"))
	if !ok {
		http.Error(w, "unknown widget", http.StatusNotFound)
		return
	}
	user, err := e.widgetUser(r)
	if err != nil {
		http.Error(w, "Open this panel in Element to use the buttons.", http.StatusUnauthorized)
		return
	}
	var body struct {
		Menu string `json:"menu"`
		Key  string `json:"key"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) != nil || body.Menu == "" || body.Key == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	accepted := false
	_ = e.s.Update(func(st *State) error {
		m := st.Members[id]
		if m == nil {
			return nil
		}
		menu := m.Menus[body.Menu]
		if menu == nil || menu.Room != room || menu.Owner != user || !menuValid(m, menu) {
			return nil
		}
		turn := "widget-" + stableID(room, body.Menu, body.Key)
		if findTurn(m, turn) != nil {
			return nil
		}
		m.Turns = append(m.Turns, &Turn{ID: turn, Room: room, Sender: user, Text: "!menu-choice", MenuID: body.Menu, Choice: body.Key, Kind: "command", Phase: "queued", Created: time.Now().UTC()})
		accepted = true
		return nil
	})
	if !accepted {
		http.Error(w, "This menu is closed, stale, or belongs to another user.", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonBytes(map[string]any{"ok": true}))
}

// widgetUser resolves the person pressing the button from their own access token.
func (e *Engine) widgetUser(r *http.Request) (string, error) {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		return "", errors.New("no access token supplied")
	}
	var out struct {
		UserID string `json:"user_id"`
	}
	if err := api(r.Context(), "GET", e.mx.Base, "/_matrix/client/v3/account/whoami", token, nil, &out, nil); err != nil {
		return "", err
	}
	if out.UserID == "" {
		return "", errors.New("access token resolved to no user")
	}
	return out.UserID, nil
}

// ensureWidget publishes the room's widget once per process. The state event carries the
// widget URL, whose fragment holds the room's widget token.
func (e *Engine) ensureWidget(ctx context.Context, id, room string) {
	if room == "" {
		return
	}
	e.widgetMu.Lock()
	installed := e.widgets[room]
	e.widgetMu.Unlock()
	if installed {
		return
	}
	view := e.s.View()
	m := view.Members[id]
	if m == nil || m.Rooms[room] == nil {
		return
	}
	token := m.Rooms[room].WidgetToken
	if token == "" {
		token = randomSecret()
		if err := e.s.Update(func(st *State) error {
			r := st.Members[id].Rooms[room]
			if r == nil {
				return nil
			}
			if r.WidgetToken == "" {
				r.WidgetToken = token
			}
			return nil
		}); err != nil {
			return
		}
	}
	address := e.c.PublicURL + "/widget/fleet?room=" + url.QueryEscape(room) + "#k=" + url.QueryEscape(token)
	if err := e.mx.installWidget(ctx, id, room, address); err != nil {
		return
	}
	e.widgetMu.Lock()
	if e.widgets == nil {
		e.widgets = map[string]bool{}
	}
	e.widgets[room] = true
	e.widgetMu.Unlock()
}
