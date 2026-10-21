package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type FleetTemplate struct{ Name, Kind, Operation string }
type fleetOwner struct{ Operation, Identity string }

func fleetDirectoryIdentity(dir string) (string, error) {
	b, err := readPrivate(filepath.Join(dir, ".fleet-owner.json"))
	if os.IsNotExist(err) {
		return filepath.Base(dir), nil
	}
	var owner fleetOwner
	if err != nil || json.Unmarshal(b, &owner) != nil || owner.Operation == "" || !localpart.MatchString(owner.Identity) || owner.Identity == fleetID {
		return "", errors.New("invalid fleet ownership record")
	}
	return owner.Identity, nil
}

func fleetDirectoryPort(dir string) (int, error) {
	if err := realDirectory(filepath.Join(dir, "data")); err != nil {
		return 0, err
	}
	b, err := readPrivate(filepath.Join(dir, "data", ".env"))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, errors.New("cannot read agent API configuration")
	}
	port, seen := 0, false
	quote := byte(0)
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
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
		value := strings.TrimSpace(match[2])
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			end := quoteEnd(value, value[0])
			if end < 0 {
				quote = value[0]
				value = ""
			} else {
				value = value[1:end]
			}
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		if match[1] != "API_SERVER_PORT" {
			continue
		}
		port, err = strconv.Atoi(value)
		if seen || err != nil || port < 1024 || port > 65535 {
			return 0, errors.New("invalid or duplicate persisted API port")
		}
		seen = true
	}
	return port, nil
}

func restoreFleetDirectorySettings(m *Member) error {
	b, err := readPrivate(filepath.Join(m.Dir, ".fleet-availability.json"))
	if !os.IsNotExist(err) {
		var availability struct{ Paused bool }
		if err != nil || json.Unmarshal(b, &availability) != nil {
			return errors.New("invalid agent availability record")
		}
		m.Paused = availability.Paused
	}
	settings, err := readFleetSettings(m.Dir)
	if err != nil {
		return err
	}
	m.DefaultModel, m.DefaultProvider, m.InitialSessions = settings.DefaultModel, settings.DefaultProvider, settings.Sessions
	return nil
}

type fleetSettings struct {
	DefaultModel, DefaultProvider string
	Sessions                      map[string]*Session
}

func snapshotFleetSettings(m *Member) (fleetSettings, error) {
	settings := fleetSettings{DefaultModel: m.DefaultModel, DefaultProvider: m.DefaultProvider, Sessions: map[string]*Session{}}
	selected := ""
	if session := m.Sessions[m.Room][selectedKey(m, m.Room)]; session != nil {
		selected = session.ID
	}
	for _, sessions := range m.Sessions {
		for _, session := range sessions {
			key := "chat:" + stableID(session.ID)
			if session.ID == selected {
				key = "room"
			}
			settings.Sessions[key] = session
		}
	}
	if len(settings.Sessions) > maxConversations {
		return settings, errors.New("full snapshot exceeds the new room's 1000-conversation limit; no conversation mappings were dropped")
	}
	return settings, nil
}

func readFleetSettings(dir string) (fleetSettings, error) {
	var settings fleetSettings
	p := filepath.Join(dir, "fleet-settings.json")
	i, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return settings, nil
	}
	if err != nil || !i.Mode().IsRegular() || i.Size() > 8<<20 {
		return settings, errors.New("snapshot bridge settings are unavailable or unsafe")
	}
	b, err := os.ReadFile(p)
	if err != nil || json.Unmarshal(b, &settings) != nil || len(settings.Sessions) > maxConversations {
		return settings, errors.New("snapshot bridge settings are invalid or oversized")
	}
	for key, session := range settings.Sessions {
		if session == nil || session.ID == "" || (key != "room" && !strings.HasPrefix(key, "chat:")) {
			return settings, errors.New("invalid snapshot conversation mapping")
		}
	}
	return settings, nil
}

func (d *Docker) fleetProjectEmpty(ctx context.Context, m *Member) error {
	if err := d.checkProject(ctx, m); err != nil {
		return err
	}
	var containers []struct {
		Labels map[string]string `json:"Labels"`
	}
	if err := d.get(ctx, "/containers/json?all=1", &containers); err != nil {
		return err
	}
	for _, container := range containers {
		if container.Labels["com.docker.compose.project"] == m.Project {
			return errors.New("project still has containers; refusing to copy or erase its storage")
		}
	}
	return nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// Reject symlinked ancestors too: checking only the leaf is not a deletion boundary.
func realDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("storage path must be a clean absolute directory")
	}
	current := string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(path, current), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		i, err := os.Lstat(current)
		if err != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
			return errors.New("storage directory or ancestor is missing or symlinked")
		}
	}
	return nil
}

func fleetStorage(c Config) error {
	for _, root := range []string{c.FleetDir, c.TemplateDir} {
		if root == "" || root == "/" {
			return errors.New("fleet storage is not configured")
		}
		if err := realDirectory(filepath.Dir(root)); err != nil {
			return err
		}
		if err := os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
			return errors.New("cannot create fleet storage directory")
		}
		if err := realDirectory(root); err != nil {
			return err
		}
	}
	return nil
}

func managedPath(root, path string) error {
	if filepath.Dir(path) != root || !fleetName(filepath.Base(path)) {
		return errors.New("operation target must be an immediate named child of managed storage")
	}
	if err := realDirectory(root); err != nil {
		return err
	}
	return realDirectory(path)
}

func rejectFleetMounts(dir string) error {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return errors.New("cannot verify managed storage mount boundaries")
	}
	return validateFleetMounts(dir, string(b))
}

func validateFleetMounts(dir, mountinfo string) error {
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if pathWithin(dir, unescape.Replace(fields[4])) {
			return errors.New("agent/template directory contains an independent mount; mount only the fleet/template parents, not individual agents or nested storage")
		}
	}
	return nil
}

func readFleetTemplate(root, name string) (FleetTemplate, error) {
	var tpl FleetTemplate
	if !fleetName(name) {
		return tpl, errors.New("invalid template name")
	}
	dir := filepath.Join(root, name)
	if err := managedPath(root, dir); err != nil {
		return tpl, errors.New("template directory is unavailable or unsafe")
	}
	p := filepath.Join(dir, "fleet-template.json")
	i, err := os.Lstat(p)
	if err != nil || !i.Mode().IsRegular() || i.Size() > 4096 {
		return tpl, errors.New("template requires a regular fleet-template.json manifest")
	}
	b, err := os.ReadFile(p)
	if err != nil || json.Unmarshal(b, &tpl) != nil || (tpl.Kind != "Clean" && tpl.Kind != "Full snapshot") {
		return tpl, errors.New("invalid template manifest: Kind must be Clean or Full snapshot")
	}
	tpl.Name = name
	return tpl, nil
}

func listFleetTemplates(root string) []FleetTemplate {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []FleetTemplate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if tpl, err := readFleetTemplate(root, entry.Name()); err == nil {
			out = append(out, tpl)
		}
		if len(out) >= 1000 {
			break
		}
	}
	return out
}

func (d *Docker) fleetCompose(ctx context.Context, m *Member) (map[string]any, error) {
	b, err := d.compose(ctx, m, "config", "--format", "json", "--no-env-resolution")
	if err != nil {
		return nil, err
	}
	var config map[string]any
	if json.Unmarshal(b, &config) != nil {
		return nil, errors.New("invalid managed Compose JSON")
	}
	return config, nil
}

func (d *Docker) validateManaged(ctx context.Context, c Config, m *Member) error {
	if err := managedPath(c.FleetDir, m.Dir); err != nil {
		return err
	}
	if err := realDirectory(m.Home); err != nil {
		return err
	}
	if err := rejectFleetMounts(m.Dir); err != nil {
		return err
	}
	if m.Home != filepath.Join(m.Dir, "data") {
		return errors.New("managed Hermes home must be its own data directory")
	}
	for _, file := range m.Files {
		i, err := os.Lstat(file)
		if filepath.Dir(file) != m.Dir || err != nil || !i.Mode().IsRegular() {
			return errors.New("managed Compose files must be regular files inside the agent directory")
		}
	}
	config, err := d.fleetCompose(ctx, m)
	if err != nil {
		return err
	}
	return validateFleetCompose(config, m.Dir)
}

func validateFleetCompose(config map[string]any, dir string) error {
	services, ok := config["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return errors.New("managed Compose services missing")
	}
	for _, raw := range services {
		s, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid managed service")
		}
		if volumes, ok := s["volumes"].([]any); ok {
			for _, raw := range volumes {
				v, ok := raw.(map[string]any)
				if !ok {
					return errors.New("managed mount must use resolved long syntax")
				}
				typeName, _ := v["type"].(string)
				if typeName == "tmpfs" {
					continue
				}
				if typeName != "bind" {
					return errors.New("managed agents require bind mounts; named/anonymous volumes cannot be safely copied or erased")
				}
				source, _ := v["source"].(string)
				readOnly, _ := v["read_only"].(bool)
				if !filepath.IsAbs(source) {
					return errors.New("managed bind sources must resolve to absolute paths")
				}
				if !pathWithin(dir, source) && !readOnly {
					return errors.New("writable agent storage must be self-contained; move external/shared writable mounts into its managed directory")
				}
				if pathWithin(dir, source) {
					resolved, err := filepath.EvalSymlinks(source)
					if err != nil || !pathWithin(dir, resolved) {
						return errors.New("managed bind source is missing or escapes through a symlink")
					}
				}
			}
		}
	}
	return nil
}

// Full snapshots preserve regular files and internal symlinks, never external symlinks or devices.
func copyFleetTree(ctx context.Context, src, dst string) error {
	if err := realDirectory(src); err != nil {
		return err
	}
	if err := realDirectory(dst); err != nil {
		return err
	}
	if err := rejectFleetMounts(src); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("cannot read snapshot source")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == ".fleet-owner.json" || rel == "fleet-template.json" {
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return errors.New("cannot inspect snapshot file")
		}
		switch {
		case entry.IsDir():
			return os.Mkdir(target, 0700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return errors.New("cannot read snapshot symlink")
			}
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || !pathWithin(src, resolved) {
				return errors.New("snapshot contains an external or broken symlink; refusing incomplete/unsafe copy")
			}
			if filepath.IsAbs(link) {
				link = filepath.Join(dst, strings.TrimPrefix(strings.TrimPrefix(link, src), string(os.PathSeparator)))
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			in, err := os.Open(path)
			if err != nil {
				return errors.New("cannot open snapshot file")
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm()&0700)
			if err != nil {
				return errors.New("cannot create snapshot file")
			}
			_, err = io.Copy(out, &fleetCopyReader{ctx: ctx, r: in})
			if err == nil {
				err = out.Sync()
			}
			closeErr := out.Close()
			if err != nil || closeErr != nil {
				return errors.New("snapshot write failed; check storage space and permissions")
			}
			return nil
		default:
			return errors.New("snapshot contains a socket, device or other special file; stop its writers and remove the transient file before retrying")
		}
	})
}

type fleetCopyReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *fleetCopyReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

func deleteFleetTree(root, dir string) error {
	if err := realDirectory(root); err != nil {
		return err
	}
	if filepath.Dir(dir) != root || !fleetName(filepath.Base(dir)) {
		return errors.New("refusing deletion outside a named managed directory")
	}
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil
	}
	if err := managedPath(root, dir); err != nil {
		return err
	}
	if err := rejectFleetMounts(dir); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return errors.New("managed directory deletion failed; retry after fixing permissions")
	}
	return syncFleetDirectory(root)
}

func syncFleetDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return errors.New("cannot open storage directory for durable sync")
	}
	defer d.Close()
	if d.Sync() != nil {
		return errors.New("storage directory sync failed")
	}
	return nil
}

func relocateFleetConfig(value any, src, dst string) any {
	switch v := value.(type) {
	case string:
		if filepath.IsAbs(v) && pathWithin(src, v) {
			rel, _ := filepath.Rel(src, v)
			return filepath.Join(dst, rel)
		}
		return v
	case []any:
		for i, x := range v {
			v[i] = relocateFleetConfig(x, src, dst)
		}
	case map[string]any:
		for k, x := range v {
			v[k] = relocateFleetConfig(x, src, dst)
		}
	}
	return value
}

func rewriteFleetCompose(config map[string]any, src, dst, identity string) error {
	relocateFleetConfig(config, src, dst)
	config["name"] = identity
	services, _ := config["services"].(map[string]any)
	for _, raw := range services {
		s := raw.(map[string]any)
		delete(s, "container_name")
		// Explicit host ports cannot be duplicated safely; Docker assigns fresh published ports.
		if ports, ok := s["ports"].([]any); ok {
			for _, raw := range ports {
				if p, ok := raw.(map[string]any); ok {
					delete(p, "published")
				}
			}
		}
	}
	for _, section := range []string{"networks", "volumes"} {
		if objects, ok := config[section].(map[string]any); ok {
			for name, raw := range objects {
				if object, ok := raw.(map[string]any); ok {
					if external, _ := object["external"].(bool); external {
						return errors.New("full copies cannot share external Compose networks or volumes; use project-owned networking")
					}
					object["name"] = identity + "_" + name
				}
			}
		}
	}
	return nil
}

func (e *Engine) publishFleetCopy(ctx context.Context, op *FleetOperation, source *Member) error {
	root := e.c.FleetDir
	if op.Action == "save-template" {
		root = e.c.TemplateDir
	}
	dst := filepath.Join(root, op.Name)
	if _, err := os.Lstat(dst); err == nil {
		b, err := os.ReadFile(filepath.Join(dst, ".fleet-owner.json"))
		var owner fleetOwner
		if err != nil || json.Unmarshal(b, &owner) != nil || owner.Operation != op.ID || owner.Identity != op.Target {
			return errors.New("destination already exists and is not owned by this operation; nothing overwritten")
		}
		return e.registerFleetCopy(ctx, op, dst)
	} else if !os.IsNotExist(err) {
		return errors.New("cannot inspect copy destination")
	}
	src := ""
	if source != nil {
		src = source.Dir
	} else {
		tpl, err := readFleetTemplate(e.c.TemplateDir, op.Source)
		if err != nil {
			return err
		}
		src = filepath.Join(e.c.TemplateDir, tpl.Name)
	}
	// Configuration is resolved by Compose, not by a home-grown YAML parser.
	var m *Member
	var err error
	if source != nil {
		m = source
	} else {
		m, err = e.docker.discover(ctx, src, Override{})
		if err != nil {
			return err
		}
	}
	config, err := e.docker.fleetCompose(ctx, m)
	if err != nil {
		return err
	}
	if err = validateFleetCompose(config, src); err != nil {
		return err
	}
	if err = rewriteFleetCompose(config, src, dst, op.Target); err != nil {
		return err
	}
	if err = realDirectory(root); err != nil {
		return err
	}
	stage := filepath.Join(root, ".fleet-"+stableID(op.ID)[:24])
	if _, err = os.Lstat(stage); err == nil {
		if err = realDirectory(stage); err != nil {
			return err
		}
		b, readErr := os.ReadFile(filepath.Join(stage, ".fleet-owner.json"))
		var owner fleetOwner
		if readErr != nil || json.Unmarshal(b, &owner) != nil || owner.Operation != op.ID || owner.Identity != op.Target {
			return errors.New("staging ownership is missing or differs; inspect locally rather than overwriting it")
		}
		if err = os.RemoveAll(stage); err != nil {
			return errors.New("cannot clear incomplete operation staging directory")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("cannot inspect staging directory")
	}
	if err = os.Mkdir(stage, 0700); err != nil {
		return errors.New("cannot create snapshot staging directory")
	}
	if err = atomicFile(filepath.Join(stage, ".fleet-owner.json"), jsonBytes(fleetOwner{Operation: op.ID, Identity: op.Target})); err != nil {
		return err
	}
	if err = copyFleetTree(ctx, src, stage); err != nil {
		return err
	}
	if source != nil {
		settings, err := snapshotFleetSettings(source)
		if err != nil {
			return err
		}
		if err = atomicFile(filepath.Join(stage, "fleet-settings.json"), jsonBytes(settings)); err != nil {
			return err
		}
	}
	// Internal absolute symlinks must refer to the final location, not staging.
	if err = filepath.WalkDir(stage, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) && pathWithin(stage, link) {
				rel, _ := filepath.Rel(stage, link)
				if err = os.Remove(path); err != nil {
					return err
				}
				return os.Symlink(filepath.Join(dst, rel), path)
			}
		}
		return nil
	}); err != nil {
		return errors.New("cannot finalize internal snapshot symlinks")
	}
	if err = atomicFile(filepath.Join(stage, "fleet-compose.json"), jsonBytes(config)); err != nil {
		return err
	}
	if err = atomicFile(filepath.Join(stage, ".fleet-owner.json"), jsonBytes(fleetOwner{Operation: op.ID, Identity: op.Target})); err != nil {
		return err
	}
	if op.Action == "save-template" {
		if err = atomicFile(filepath.Join(stage, "fleet-template.json"), jsonBytes(FleetTemplate{Name: op.Name, Kind: "Full snapshot", Operation: op.ID})); err != nil {
			return err
		}
	} else {
		host := "0.0.0.0"
		if m.Network == "host" {
			host = "127.0.0.1"
		}
		if _, err = configureEnvKey(filepath.Join(stage, "data", ".env"), host, op.Port, true); err != nil {
			return err
		}
		// A published agent is ready to receive work. The record travels with the
		// directory so recovery never has to guess the admission state.
		if err = atomicFile(filepath.Join(stage, ".fleet-availability.json"), jsonBytes(struct{ Paused bool }{false})); err != nil {
			return err
		}
	}
	var directories []string
	if err = filepath.WalkDir(stage, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return errors.New("cannot inspect completed snapshot")
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err = syncFleetDirectory(directories[i]); err != nil {
			return err
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if _, err = os.Lstat(dst); !os.IsNotExist(err) {
		return errors.New("copy destination appeared during snapshot; refusing replacement")
	}
	if err = syncFleetDirectory(stage); err != nil {
		return err
	}
	if err = os.Rename(stage, dst); err != nil {
		return errors.New("cannot publish snapshot; destination is never overwritten")
	}
	if err = syncFleetDirectory(root); err != nil {
		return err
	}
	return e.registerFleetCopy(ctx, op, dst)
}

func (e *Engine) registerFleetCopy(ctx context.Context, op *FleetOperation, dir string) error {
	if op.Action == "save-template" {
		return nil
	}
	if e.s.View().Members[op.Target] != nil {
		return nil
	}
	m, err := e.docker.discover(ctx, dir, Override{Project: op.Target, Port: op.Port})
	if err != nil {
		return err
	}
	m.ID, m.Name, m.Managed = op.Target, op.Name, true
	b, err := os.ReadFile(filepath.Join(dir, ".fleet-availability.json"))
	var availability struct{ Paused bool }
	if err != nil || json.Unmarshal(b, &availability) != nil {
		return errors.New("published agent availability record is missing or corrupt")
	}
	m.Paused = availability.Paused
	settings, err := readFleetSettings(dir)
	if err != nil {
		return err
	}
	m.DefaultModel, m.DefaultProvider, m.InitialSessions = settings.DefaultModel, settings.DefaultProvider, settings.Sessions
	if err = e.docker.validateManaged(ctx, e.c, m); err != nil {
		return err
	}
	if err = e.docker.checkProject(ctx, m); err != nil {
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
	if err = e.mx.rememberKey(m.ID, key); err != nil {
		return err
	}
	return e.s.Update(func(st *State) error {
		if st.Members[m.ID] == nil {
			st.Members[m.ID] = m
		}
		return nil
	})
}
