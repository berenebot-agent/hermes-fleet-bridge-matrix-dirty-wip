package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
)

func frontend(c Config, s *Store, ready *atomic.Bool) http.Handler {
	target, _ := url.Parse("http://" + c.MatrixListen)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "Homeserver unavailable", http.StatusBadGateway)
	}
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		for _, k := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP"} {
			r.Header.Del(k)
		}
		director(r)
		r.Host = target.Host
	}
	files := http.FileServer(http.Dir(c.Assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		p := r.URL.Path
		if p == "/healthz" {
			state := s.View()
			held, blocked, syncFailed := 0, 0, 0
			for _, m := range state.Members {
				if m.Deleted {
					continue
				}
				if m.SyncFailed {
					syncFailed++
				}
				for _, t := range m.Turns {
					if t.Phase == "held" {
						held++
					}
				}
				for _, n := range m.Outbox {
					if n.Attempts >= 8 {
						blocked++
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			if !ready.Load() || syncFailed > 0 || blocked > 0 || held > 0 {
				w.WriteHeader(503)
			}
			w.Write(jsonBytes(map[string]any{"ready": ready.Load(), "held_turns": held, "blocked_replies": blocked, "sync_failed_agents": syncFailed}))
			return
		}
		if !ready.Load() {
			http.Error(w, "Initializing private chat; retry shortly", 503)
			return
		}
		if p == "/.well-known/matrix/client" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Write(jsonBytes(map[string]any{"m.homeserver": map[string]string{"base_url": c.PublicURL}, "io.element.e2ee": map[string]bool{"default": true}}))
			return
		}
		if p == "/config.json" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.Write(jsonBytes(map[string]any{"brand": "Hermes Fleet", "default_server_config": map[string]any{"m.homeserver": map[string]string{"base_url": c.PublicURL, "server_name": c.ServerName}}, "disable_custom_urls": true, "disable_guests": true, "disable_registration": true, "default_theme": "dark", "show_labs_settings": false, "integrations_ui_url": "", "integrations_rest_url": "", "integrations_widgets_urls": []string{}, "room_directory": map[string]any{"servers": []string{}}, "setting_defaults": map[string]any{"UIFeature.identityServer": false}, "mobile_guide_toast": false}))
			return
		}
		if strings.HasPrefix(p, "/_matrix/") {
			if path.Clean(p) != strings.TrimSuffix(p, "/") || strings.Contains(p, "/admin/") || !(strings.HasPrefix(p, "/_matrix/client/") || strings.HasPrefix(p, "/_matrix/media/")) {
				http.NotFound(w, r)
				return
			}
			proxy.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(p, "/_") || strings.HasPrefix(p, "/.well-known/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		files.ServeHTTP(w, r)
	})
}
