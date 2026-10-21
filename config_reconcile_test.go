package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPublicURLChangePreservesStateAndPersists(t *testing.T) {
	c := testConfig(t)
	s, err := openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(st *State) error {
		st.Members["iris"] = &Member{ID: "iris", Enabled: true, Room: "!room:chat.test", Rooms: map[string]*Room{"!room:chat.test": {}}, Sessions: map[string]map[string]*Session{}, Turns: []*Turn{{ID: "event", Room: "!room:chat.test", Phase: "running", RunID: "existing-run"}}}
		return nil
	}); err != nil {
		s.Close()
		t.Fatal(err)
	}
	before := s.View()
	s.Close()
	c.PublicURL = "https://new-chat.test"
	s, err = openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	after := s.View()
	s.Close()
	if after.PublicURL != c.PublicURL || after.ServerName != before.ServerName || after.Admin != before.Admin || !reflect.DeepEqual(after.Members, before.Members) {
		t.Fatal("URL migration changed durable identities, work or rooms")
	}
	s, err = openStore(c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.View().PublicURL != c.PublicURL {
		t.Fatal("new URL not durable")
	}
	i, err := os.Stat(filepath.Join(c.Data, "controller/state.json"))
	if err != nil || i.Mode().Perm() != 0600 {
		t.Fatal("state permissions weakened")
	}
}

func TestPublicURLChangeDoesNotPermitIdentityRename(t *testing.T) {
	for _, field := range []string{"server", "admin"} {
		t.Run(field, func(t *testing.T) {
			c := testConfig(t)
			s, err := openStore(c)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			path := filepath.Join(c.Data, "controller/state.json")
			before, _ := os.ReadFile(path)
			c.PublicURL = "https://new-chat.test"
			if field == "server" {
				c.ServerName = "new-chat.test"
			} else {
				c.Admin = "another-admin"
			}
			if next, err := openStore(c); err == nil {
				next.Close()
				t.Fatal("identity rename accepted")
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("rejected rename modified state")
			}
		})
	}
}

func TestFrontendSeparatesPublicOriginAndServerIdentity(t *testing.T) {
	c, s := testStore(t)
	c.PublicURL = "https://new-chat.test"
	var ready atomic.Bool
	ready.Store(true)
	h := frontend(c, s, &ready)
	for _, path := range []string{"/config.json", "/.well-known/matrix/client"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var body map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatal("frontend discovery failed")
		}
		if path == "/config.json" {
			body = body["default_server_config"].(map[string]any)
		}
		home := body["m.homeserver"].(map[string]any)
		if home["base_url"] != c.PublicURL || (path == "/config.json" && home["server_name"] != c.ServerName) {
			t.Fatal("public origin and immutable server identity conflated")
		}
	}
}

func TestAdministratorPasswordReconciliation(t *testing.T) {
	const old = "old-admin-password-fixture"
	const desired = "new-admin-password-fixture"
	for _, test := range []struct {
		name                                                                                                             string
		expired, alreadyApplied, lostResponse, failLoginAfterReset, rejectReset, wrongIdentity, failSave, unchanged, bot bool
	}{
		{name: "valid saved token"},
		{name: "expired token", expired: true},
		{name: "remote already applied", alreadyApplied: true},
		{name: "lost reset response", lostResponse: true},
		{name: "restart after unverified remote change", failLoginAfterReset: true},
		{name: "remote refuses reset", rejectReset: true},
		{name: "wrong authenticated identity", wrongIdentity: true},
		{name: "restart after failed secrets save", failSave: true},
		{name: "unchanged password", unchanged: true},
		{name: "bot credentials unchanged", bot: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testConfig(t)
			c.Password = desired
			if test.unchanged {
				c.Password = old
			}
			s, err := openStore(c)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
			mx, err := newMatrix(c)
			if err != nil {
				t.Fatal(err)
			}
			name := c.Admin
			if test.bot {
				name = "agent_iris"
			}
			user := "@" + name + ":" + c.ServerName
			mx.secrets.Accounts[name] = &Credential{Password: old, Token: "saved-private-token", User: user}
			key := randomSecret()
			mx.secrets.APIKeys["iris"] = key
			if err = mx.saveSecrets(); err != nil {
				t.Fatal(err)
			}
			secretPath := mx.secretPath
			before, _ := os.ReadFile(secretPath)
			var mu sync.Mutex
			password := old
			if test.alreadyApplied {
				password = desired
			}
			tokens := map[string]bool{"saved-private-token": !test.expired}
			resets, logins, registers := 0, 0, 0
			failLogin := test.failLoginAfterReset
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				switch {
				case r.URL.Path == "/_matrix/client/v3/login":
					logins++
					var body struct {
						Password   string
						Identifier struct{ User string }
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.Identifier.User != name || body.Password != password {
						http.Error(w, "invalid credential", 403)
						return
					}
					if resets > 0 && failLogin {
						failLogin = false
						http.Error(w, "verification unavailable", 503)
						return
					}
					issued := "verified-login-token"
					tokens[issued] = true
					replyJSON(w, map[string]string{"user_id": user, "access_token": issued})
				case r.URL.Path == "/_matrix/client/v3/account/whoami":
					if !tokens[token] {
						http.Error(w, "expired", 401)
						return
					}
					who := user
					if test.wrongIdentity {
						who = "@wrong:chat.test"
					}
					replyJSON(w, map[string]string{"user_id": who})
				case strings.HasPrefix(r.URL.Path, "/_synapse/admin/v1/reset_password/"):
					if !tokens[token] || r.URL.Path != "/_synapse/admin/v1/reset_password/"+user {
						t.Error("password reset was not authenticated for the exact administrator")
						http.Error(w, "denied", 403)
						return
					}
					var body struct {
						Password string `json:"new_password"`
						Logout   bool   `json:"logout_devices"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.Password != desired || !body.Logout {
						t.Error("unsafe password reset request")
					}
					resets++
					if test.rejectReset {
						http.Error(w, desired+" "+old, 503)
						return
					}
					password = body.Password
					tokens = map[string]bool{}
					if test.lostResponse {
						http.Error(w, "response lost after application", 503)
						return
					}
					replyJSON(w, map[string]any{})
				default:
					registers++
					http.Error(w, "must not register an existing administrator", 403)
				}
			}))
			defer server.Close()
			mx.Base = server.URL
			if test.failSave {
				mx.secretPath = filepath.Join(c.Data, "missing", "accounts.json")
			}
			account, err := mx.account(context.Background(), name, !test.bot)
			wantFailure := test.failLoginAfterReset || test.rejectReset || test.wrongIdentity || test.failSave
			if (err != nil) != wantFailure {
				t.Fatal("unexpected password reconciliation outcome")
			}
			if err != nil {
				if strings.Contains(err.Error(), old) || strings.Contains(err.Error(), desired) || strings.Contains(err.Error(), "saved-private-token") {
					t.Fatal("error exposed credential")
				}
				after, _ := os.ReadFile(secretPath)
				if string(after) != string(before) || mx.cred(name).Password != old {
					t.Fatal("failed change overwrote last private credential record")
				}
				if test.failLoginAfterReset || test.failSave {
					mx, err = newMatrix(c)
					if err != nil {
						t.Fatal(err)
					}
					mx.Base = server.URL
					account, err = mx.account(context.Background(), name, true)
					if err != nil || account.Password != desired {
						t.Fatal("restart did not reconcile already-applied password")
					}
				}
			} else {
				want := desired
				if test.unchanged || test.bot {
					want = old
				}
				if account.Password != want || account.User != user {
					t.Fatal("password/account identity not reconciled")
				}
				persisted, err := newMatrix(c)
				if err != nil || persisted.cred(name).Password != want {
					t.Fatal("verified password not saved durably")
				}
			}
			if mx.key("iris") != key {
				t.Fatal("administrator password change affected agent API credentials")
			}
			mu.Lock()
			defer mu.Unlock()
			if registers != 0 {
				t.Fatal("password reconciliation attempted account registration")
			}
			wantResets := 1
			if test.alreadyApplied || test.wrongIdentity || test.unchanged || test.bot {
				wantResets = 0
			}
			if resets != wantResets {
				t.Fatal("password reset missing or repeated after recovery")
			}
			if (test.unchanged || test.bot) && logins != 0 {
				t.Fatal("unchanged credentials performed unnecessary login")
			}
		})
	}
}
