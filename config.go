package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	PublicURL, ServerName, Admin, Password, Data, Listen, MatrixListen, Assets string
	Dirs                                                                       []string
	Overrides                                                                  map[string]Override
	Idle, Start                                                                time.Duration
	Parallel                                                                   int
}
type Override struct {
	Service string `json:"service"`
	Project string `json:"project"`
	Network string `json:"network"`
	Port    int    `json:"port"`
}

var localpart = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

func env(k, d string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return d
}
func loadConfig() (c Config, err error) {
	c.PublicURL = env("PUBLIC_URL", "")
	u, e := url.Parse(c.PublicURL)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return c, errors.New("PUBLIC_URL must be an HTTPS origin without credentials, path, query or fragment")
	}
	c.PublicURL = strings.TrimSuffix(c.PublicURL, "/")
	c.ServerName = env("MATRIX_SERVER_NAME", u.Hostname())
	if c.ServerName != u.Hostname() {
		return c, errors.New("MATRIX_SERVER_NAME must match the PUBLIC_URL hostname in this single-origin MVP")
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`).MatchString(c.ServerName) || strings.Contains(c.ServerName, "..") {
		return c, errors.New("MATRIX_SERVER_NAME must be a lowercase DNS hostname")
	}
	c.Admin = strings.ToLower(strings.TrimSpace(env("ADMIN_USERNAME", "admin")))
	c.Password = os.Getenv("ADMIN_PASSWORD")
	// The homeserver lowercases localparts, so a mixed-case ADMIN_USERNAME would register a
	// different account than the one the password is later checked against. Normalise instead of
	// refusing, and treat the reserved namespace case-insensitively.
	if !localpart.MatchString(c.Admin) || strings.HasPrefix(c.Admin, "agent_") || strings.HasPrefix(c.Admin, "agent.") || strings.HasPrefix(c.Admin, "agent-") {
		return c, errors.New("ADMIN_USERNAME must be a localpart outside the reserved agent_ namespace")
	}
	if len(c.Password) < 16 || strings.ContainsAny(c.Password, "\x00\r\n") {
		return c, errors.New("ADMIN_PASSWORD must contain at least 16 characters and no line breaks")
	}
	c.Data = env("DATA_DIR", "/data")
	if !filepath.IsAbs(c.Data) {
		return c, errors.New("DATA_DIR must be absolute")
	}
	c.Listen = env("LISTEN_ADDR", "0.0.0.0:8088")
	c.MatrixListen = env("MATRIX_LISTEN_ADDR", "127.0.0.1:18008")
	host, _, e := net.SplitHostPort(c.MatrixListen)
	if e != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return c, errors.New("MATRIX_LISTEN_ADDR must use a loopback IP and port")
	}
	if _, _, e = net.SplitHostPort(c.Listen); e != nil || c.Listen == c.MatrixListen {
		return c, errors.New("invalid LISTEN_ADDR")
	}
	c.Assets = env("ELEMENT_DIR", "/opt/element")
	c.Idle, e = time.ParseDuration(env("IDLE_TIMEOUT", "15m"))
	if e != nil || c.Idle < time.Second {
		return c, errors.New("invalid IDLE_TIMEOUT")
	}
	c.Start, e = time.ParseDuration(env("START_TIMEOUT", "120s"))
	if e != nil || c.Start < time.Second {
		return c, errors.New("invalid START_TIMEOUT")
	}
	c.Parallel, e = strconv.Atoi(env("MAX_PARALLEL_RUNS", "4"))
	if e != nil || c.Parallel < 1 || c.Parallel > 32 {
		return c, errors.New("MAX_PARALLEL_RUNS must be 1..32")
	}
	c.Overrides = map[string]Override{}
	if s := os.Getenv("HERMES_OVERRIDES"); s != "" {
		if json.Unmarshal([]byte(s), &c.Overrides) != nil {
			return c, errors.New("HERMES_OVERRIDES must be a JSON object")
		}
	}
	seen := map[string]bool{}
	for _, p := range strings.Split(os.Getenv("HERMES_DIRS"), ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			return c, errors.New("HERMES_DIRS entries must be absolute")
		}
		p, e = filepath.EvalSymlinks(p)
		if e != nil {
			return c, errors.New("a configured member directory is unavailable")
		}
		id := filepath.Base(p)
		if !localpart.MatchString(id) || seen[id] {
			return c, errors.New("member basenames must be unique lowercase Matrix-safe localparts")
		}
		seen[id] = true
		c.Dirs = append(c.Dirs, p)
	}
	for id := range c.Overrides {
		if !seen[id] {
			return c, errors.New("HERMES_OVERRIDES names an unconfigured agent")
		}
	}
	return c, nil
}
func randomSecret() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic("secure randomness unavailable")
	}
	return hex.EncodeToString(b)
}
func stableID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}
func strongKey(s string) bool {
	distinct := map[rune]bool{}
	for _, r := range s {
		if r < 33 || r > 126 || strings.ContainsRune(`$"'\`, r) {
			return false
		}
		distinct[r] = true
	}
	return len(s) >= 32 && len(distinct) >= 8
}
func jsonBytes(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(fmt.Sprintf("internal JSON encoding failure: %T", v))
	}
	return b
}
