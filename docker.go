package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Docker struct {
	client *http.Client
	Socket string
}

var errStopped = errors.New("Hermes service is stopped")

func newDocker() *Docker {
	socket := "/var/run/docker.sock"
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	return &Docker{Socket: socket, client: &http.Client{Transport: tr, Timeout: 15 * time.Second}}
}
func (d *Docker) get(ctx context.Context, path string, out any) error {
	r, e := http.NewRequestWithContext(ctx, "GET", "http://docker"+path, nil)
	if e != nil {
		return stateError("Docker request")
	}
	res, e := d.client.Do(r)
	if e != nil {
		return errors.New("Docker socket unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return errors.New("Docker inspection failed")
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if e != nil || len(b) > 8<<20 || json.Unmarshal(b, out) != nil {
		return errors.New("Docker inspection invalid or oversized")
	}
	return nil
}

// post sends a Docker API request that expects no response body.
func (d *Docker) post(ctx context.Context, path string) error {
	r, e := http.NewRequestWithContext(ctx, "POST", "http://docker"+path, nil)
	if e != nil {
		return stateError("Docker request")
	}
	res, e := d.client.Do(r)
	if e != nil {
		return errors.New("Docker socket unavailable")
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errors.New("Docker request failed")
	}
	return nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > b.limit {
		return 0, errors.New("command output exceeds safety limit")
	}
	return b.Buffer.Write(p)
}
func (d *Docker) compose(ctx context.Context, m *Member, command ...string) ([]byte, error) {
	args := []string{"--host", "unix://" + d.Socket, "compose", "--project-directory", m.Dir}
	for _, f := range m.Files {
		args = append(args, "--file", f)
	}
	if m.Project != "" {
		args = append(args, "--project-name", m.Project)
	}
	args = append(args, command...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = m.Dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/tmp", "DOCKER_CONFIG=/tmp/hfb-docker-config"}
	stdout := &cappedBuffer{limit: 8 << 20}
	stderr := &cappedBuffer{limit: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if cmd.Run() != nil {
		return nil, errors.New("Docker Compose command failed; inspect the member configuration locally (output suppressed to protect secrets)")
	}
	return stdout.Bytes(), nil
}

type composeService struct {
	NetworkMode string                                  `json:"network_mode"`
	Volumes     []struct{ Type, Source, Target string } `json:"volumes"`
	Ports       []struct {
		Target int `json:"target"`
	} `json:"ports"`
	Environment map[string]*string `json:"environment"`
}

func (d *Docker) discover(ctx context.Context, dir string, o Override) (*Member, error) {
	m := &Member{ID: filepath.Base(dir), Dir: dir, Enabled: true, Network: o.Network, Port: o.Port, Rooms: map[string]*Room{}, Sessions: map[string]map[string]*Session{}, LastActivity: time.Now().UTC()}
	for _, name := range []string{"fleet-compose.json", "compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"} {
		p := filepath.Join(dir, name)
		if i, e := os.Stat(p); e == nil && !i.IsDir() {
			m.Files = []string{p}
			base := strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
			ext := filepath.Ext(name)
			override := filepath.Join(dir, base+".override"+ext)
			if _, e = os.Stat(override); e == nil {
				m.Files = append(m.Files, override)
			}
			break
		}
	}
	if len(m.Files) == 0 {
		return nil, errors.New("member has no supported Compose file")
	}
	m.Project = o.Project
	b, e := d.compose(ctx, m, "config", "--format", "json", "--no-env-resolution")
	if e != nil {
		return nil, e
	}
	var config struct {
		Name     string                    `json:"name"`
		Services map[string]composeService `json:"services"`
	}
	if json.Unmarshal(b, &config) != nil || config.Name == "" {
		return nil, errors.New("Compose JSON configuration invalid")
	}
	m.Project = config.Name
	count := 0
	for name, s := range config.Services {
		if o.Service != "" && o.Service != name {
			continue
		}
		for _, v := range s.Volumes {
			if v.Target != "/opt/data" {
				continue
			}
			if v.Type != "bind" {
				return nil, errors.New("Hermes /opt/data must use a bind mount, never a named volume")
			}
			home, e := filepath.EvalSymlinks(v.Source)
			expected, e2 := filepath.EvalSymlinks(filepath.Join(dir, "data"))
			if e != nil || e2 != nil || home != expected {
				return nil, errors.New("Hermes /opt/data must bind the configured member's existing data/ directory")
			}
			count++
			m.Service = name
			m.Home = home
			if m.Port == 0 {
				if p := s.Environment["API_SERVER_PORT"]; p != nil {
					m.Port, _ = strconv.Atoi(*p)
				}
			}
			// Compose environment takes precedence over data/.env; conflicting values would silently break auth/binding.
			for _, key := range []string{"API_SERVER_KEY", "API_SERVER_HOST", "API_SERVER_ENABLED", "API_SERVER_PORT"} {
				if s.Environment[key] != nil {
					return nil, fmt.Errorf("remove %s from service environment; bridge manages it persistently in data/.env", key)
				}
			}
			if s.NetworkMode == "host" {
				m.Network = "host"
			}
		}
	}
	if count != 1 {
		return nil, errors.New("choose exactly one Hermes /opt/data service using HERMES_OVERRIDES when ambiguous")
	}
	if m.Port == 0 {
		m.Port = 8642
	}
	if m.Port < 1024 || m.Port > 65535 {
		return nil, errors.New("invalid Hermes API port override")
	}
	return m, nil
}

var assignment = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$`)

func quoteEnd(s string, q byte) int {
	for i := 1; i < len(s); i++ {
		if s[i] == q && s[i-1] != '\\' {
			return i
		}
	}
	return -1
}
func configureEnv(path, host string, port int) (string, error) {
	return configureEnvKey(path, host, port, false)
}
func configureEnvKey(path, host string, port int, rotate bool) (string, error) {
	info, e := os.Lstat(path)
	if e == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return "", errors.New("member data/.env must be a regular file, not a symlink")
	}
	b, e := os.ReadFile(path)
	if e != nil && !os.IsNotExist(e) {
		return "", errors.New("cannot read member data/.env")
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	values := map[string]string{}
	positions := map[string]int{}
	quote := byte(0)
	managed := map[string]string{"API_SERVER_ENABLED": "true", "API_SERVER_HOST": host, "API_SERVER_PORT": strconv.Itoa(port)}
	for i, line := range lines {
		if quote != 0 {
			if quoteEnd(string(quote)+line, quote) >= 0 {
				quote = 0
			}
			continue
		}
		match := assignment.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		key, val := match[1], strings.TrimSpace(match[2])
		multi := false
		if len(val) > 0 && (val[0] == '\'' || val[0] == '"') {
			end := quoteEnd(val, val[0])
			if end < 0 {
				quote = val[0]
				multi = true
			} else {
				val = val[1:end]
			}
		} else {
			if p := strings.Index(val, " #"); p >= 0 {
				val = strings.TrimSpace(val[:p])
			}
		}
		if _, ok := managed[key]; !ok && key != "API_SERVER_KEY" {
			continue
		}
		if multi {
			return "", errors.New("managed API_SERVER settings cannot be multiline")
		}
		if _, exists := positions[key]; exists {
			return "", errors.New("duplicate API_SERVER setting in member data/.env")
		}
		positions[key] = i
		values[key] = val
	}
	if quote != 0 {
		return "", errors.New("unterminated quoted value in member data/.env")
	}
	key := values["API_SERVER_KEY"]
	if key == "" || rotate {
		key = randomSecret()
		managed["API_SERVER_KEY"] = key
	} else if !strongKey(key) {
		return "", errors.New("existing Hermes API key is weak or uses unsupported dotenv expansion; replace it explicitly before enabling this agent")
	}
	for _, k := range []string{"API_SERVER_ENABLED", "API_SERVER_HOST", "API_SERVER_PORT", "API_SERVER_KEY"} {
		v, ok := managed[k]
		if !ok {
			continue
		}
		if p, exists := positions[k]; exists {
			lines[p] = k + "=" + v
		} else {
			lines = append(lines, k+"="+v)
		}
	}
	result := []byte(strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n")
	if !bytes.Equal(b, result) {
		if e = atomicFile(path, result); e != nil {
			return "", e
		}
	} else if e = os.Chmod(path, 0600); e != nil {
		return "", errors.New("cannot restrict member data/.env permissions")
	}
	return key, nil
}

type containerInfo struct {
	ID         string                             `json:"Id"`
	State      struct{ Running bool }             `json:"State"`
	Config     struct{ Labels map[string]string } `json:"Config"`
	HostConfig struct{ NetworkMode string }       `json:"HostConfig"`
	Mounts     []struct {
		Type   string `json:"Type"`
		Source string `json:"Source"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			IP string `json:"IPAddress"`
		}
	} `json:"NetworkSettings"`
}

func (d *Docker) checkProject(ctx context.Context, m *Member) error {
	var list []struct {
		Labels map[string]string `json:"Labels"`
	}
	if err := d.get(ctx, "/containers/json?all=1", &list); err != nil {
		return err
	}
	for _, v := range list {
		if v.Labels["com.docker.compose.project"] == m.Project && filepath.Clean(v.Labels["com.docker.compose.project.working_dir"]) != m.Dir {
			return errors.New("Compose project name collides with an unmanaged directory")
		}
	}
	return nil
}

// mountHolders lists every container that mounts anything under dir, whatever Compose project
// it belongs to. A managed agent's directory is erased by hand, so an unrelated container that
// still mounts it would keep writing into a deleted path. Project labels are not enough: a
// stray project can share the same bind mount.
func (d *Docker) mountHolders(ctx context.Context, dir string) ([]string, error) {
	var list []struct {
		ID string `json:"Id"`
	}
	if err := d.get(ctx, "/containers/json?all=1", &list); err != nil {
		return nil, err
	}
	var holders []string
	for _, v := range list {
		var info containerInfo
		if err := d.get(ctx, "/containers/"+v.ID+"/json", &info); err != nil {
			return nil, err
		}
		for _, mount := range info.Mounts {
			if mount.Type != "bind" || mount.Source == "" {
				continue
			}
			if pathWithin(dir, filepath.Clean(mount.Source)) {
				holders = append(holders, v.ID)
				break
			}
		}
	}
	return holders, nil
}

// stopMountHolders stops every container that mounts the agent's directory, including ones the
// bridge does not manage. It refuses to act on an unidentified container.
func (d *Docker) stopMountHolders(ctx context.Context, dir string) error {
	holders, err := d.mountHolders(ctx, dir)
	if err != nil {
		return err
	}
	for _, id := range holders {
		var info containerInfo
		if err := d.get(ctx, "/containers/"+id+"/json", &info); err != nil {
			return err
		}
		if info.ID == "" {
			return errors.New("cannot identify a container that mounts the agent directory; refusing to stop it")
		}
		if err := d.post(ctx, "/containers/"+id+"/stop?t=60"); err != nil {
			return err
		}
	}
	return nil
}
func (d *Docker) endpoint(ctx context.Context, m *Member) (string, error) {
	var list []struct {
		ID     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	if e := d.get(ctx, "/containers/json", &list); e != nil {
		return "", e
	}
	var found *containerInfo
	for _, v := range list {
		if v.Labels["com.docker.compose.project"] != m.Project || v.Labels["com.docker.compose.service"] != m.Service || v.Labels["com.docker.compose.oneoff"] == "True" {
			continue
		}
		var info containerInfo
		if e := d.get(ctx, "/containers/"+v.ID+"/json", &info); e != nil {
			return "", e
		}
		if filepath.Clean(info.Config.Labels["com.docker.compose.project.working_dir"]) != m.Dir {
			return "", errors.New("Compose project belongs to a different directory; refuse lifecycle control")
		}
		if found != nil {
			return "", errors.New("multiple Hermes service replicas are unsupported")
		}
		found = &info
	}
	if found == nil {
		return "", errStopped
	}
	if found.HostConfig.NetworkMode == "host" {
		return "http://127.0.0.1:" + strconv.Itoa(m.Port), nil
	}
	var ip string
	if m.Network != "" {
		n, ok := found.NetworkSettings.Networks[m.Network]
		if !ok {
			return "", errors.New("selected Hermes network not attached")
		}
		ip = n.IP
	} else {
		for _, n := range found.NetworkSettings.Networks {
			if n.IP != "" {
				if ip != "" {
					return "", errors.New("Hermes has multiple networks; configure a network override")
				}
				ip = n.IP
			}
		}
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || !parsed.IsPrivate() {
		return "", errors.New("Hermes endpoint must be a private Docker-network address")
	}
	return "http://" + net.JoinHostPort(ip, strconv.Itoa(m.Port)), nil
}
