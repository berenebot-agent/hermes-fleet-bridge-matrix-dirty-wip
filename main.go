package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

func reconcile(ctx context.Context, c Config, s *Store, mx *Matrix, d *Docker) error {
	incoming := map[string]*Member{}
	projects := map[string]bool{}
	ports := map[int]bool{}
	for _, dir := range c.Dirs {
		id := filepath.Base(dir)
		m, err := d.discover(ctx, dir, c.Overrides[id])
		if err != nil {
			return fmt.Errorf("agent %s: %w", id, err)
		}
		if projects[m.Project] {
			return errors.New("configured members share a Compose project; lifecycle must not overlap")
		}
		projects[m.Project] = true
		if m.Network == "host" {
			if ports[m.Port] {
				return errors.New("host-network Hermes agents require unique API port overrides")
			}
			ports[m.Port] = true
		}
		if err = d.checkProject(ctx, m); err != nil {
			return err
		}
		old := s.View().Members[id]
		if old != nil && pending(old) > 0 && (old.Dir != m.Dir || old.Project != m.Project || old.Service != m.Service || old.Port != m.Port || old.Network != m.Network) {
			return errors.New("member endpoint/identity changed while work is outstanding; restore the original configuration")
		}
		host := "0.0.0.0"
		if m.Network == "host" {
			host = "127.0.0.1"
		}
		key, err := configureEnv(filepath.Join(m.Home, ".env"), host, m.Port)
		if err != nil {
			return fmt.Errorf("agent %s: %w", id, err)
		}
		if err = mx.rememberKey(id, key); err != nil {
			return err
		}
		incoming[id] = m
	}
	return s.Update(func(st *State) error {
		for id, m := range st.Members {
			if incoming[id] == nil {
				m.Enabled = false
				for _, t := range m.Turns {
					if t.Phase == "queued" && t.Kind == "" {
						t.Phase = "done"
						addNotice(m, t.ID+"-removed", t.Thread, "Agent removed: this queued turn was cancelled. Active runs may finish; history is retained.")
					}
				}
			}
		}
		for id, m := range incoming {
			if old := st.Members[id]; old != nil {
				old.Dir = m.Dir
				old.Home = m.Home
				old.Project = m.Project
				old.Service = m.Service
				old.Network = m.Network
				old.Port = m.Port
				old.Files = m.Files
				old.Enabled = true
			} else {
				st.Members[id] = m
			}
		}
		return nil
	})
}
func homeserverConfig(c Config) error {
	host, port, _ := net.SplitHostPort(c.MatrixListen)
	n, _ := strconv.Atoi(port)
	config := fmt.Sprintf("[global]\nserver_name=%q\ndatabase_path=%q\naddress=[%q]\nport=%d\nregistration_shared_secret_file=%q\nallow_registration=false\nallow_federation=false\nfederate_created_rooms=false\nallow_encryption=false\nallow_public_room_directory_without_auth=false\nlockdown_public_room_directory=true\nnew_user_displayname_suffix=\"\"\ndb_cache_capacity_mb=32.0\ndb_write_buffer_capacity_mb=16.0\ncache_capacity_modifier=0.1\nlog=\"error\"\nlog_colors=false\n", c.ServerName, filepath.Join(c.Data, "matrix"), host, n, filepath.Join(c.Data, "secrets", "registration"))
	return atomicFile(filepath.Join(c.Data, "secrets", "tuwunel.toml"), []byte(config))
}
func run(ctx context.Context) error {
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(c.Assets, "index.html")); err != nil {
		return errors.New("bundled Element assets missing")
	}
	s, err := openStore(c)
	if err != nil {
		return err
	}
	defer s.Close()
	mx, err := newMatrix(c)
	if err != nil {
		return err
	}
	d := newDocker()
	if len(c.Dirs) > 0 {
		startup, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err = reconcile(startup, c, s, mx, d)
		cancel()
		if err != nil {
			return err
		}
	}
	if len(c.Dirs) == 0 {
		if err = reconcile(ctx, c, s, mx, d); err != nil {
			return err
		}
	}
	if err = homeserverConfig(c); err != nil {
		return err
	}
	child := exec.Command("/usr/bin/tuwunel", "-c", filepath.Join(c.Data, "secrets", "tuwunel.toml"))
	child.Stdout = io.Discard
	child.Stderr = io.Discard
	if err = child.Start(); err != nil {
		return errors.New("cannot start bundled Tuwunel")
	}
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()
	defer func() {
		_ = child.Process.Signal(syscall.SIGTERM)
		select {
		case <-childDone:
		case <-time.After(29 * time.Minute):
			_ = child.Process.Kill()
		}
	}()
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var ready atomic.Bool
	server := &http.Server{Addr: c.Listen, Handler: frontend(c, s, &ready), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	defer func() {
		ready.Store(false)
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	deadline := time.NewTimer(30 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		err = api(ctx, "GET", mx.Base, "/_matrix/client/versions", "", nil, nil, nil)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-childDone:
			childDone <- errors.New("homeserver stopped")
			return errors.New("Tuwunel exited before readiness; check the private homeserver configuration")
		case <-deadline.C:
			return errors.New("Tuwunel startup/migration did not finish within 30 minutes")
		case <-ticker.C:
		}
	}
	if _, err = mx.account(workCtx, c.Admin, true); err != nil {
		return err
	}
	for id, m := range s.View().Members {
		if m.Enabled {
			if err = mx.provision(workCtx, s, id); err != nil {
				return fmt.Errorf("agent %s Matrix provisioning failed: %w", id, err)
			}
		}
	}
	engine := &Engine{c: c, s: s, mx: mx, docker: d, agents: map[string]*agentRuntime{}}
	engine.Start(workCtx)
	defer func() { ready.Store(false); cancel(); engine.wg.Wait() }()
	ready.Store(true)
	log.Printf("ready: %d configured agents; private Matrix identity locked", len(c.Dirs))
	select {
	case <-ctx.Done():
		return nil
	case err = <-s.Fatal:
		return err
	case <-childDone:
		childDone <- errors.New("homeserver stopped")
		return errors.New("Tuwunel exited; stopping the bundled service")
	case err = <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("frontend listener failed")
		}
		return nil
	}
}
func main() {
	if len(os.Args) == 2 && os.Args[1] == "health" {
		client := &http.Client{Timeout: 3 * time.Second}
		addr := env("LISTEN_ADDR", "0.0.0.0:8088")
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			os.Exit(1)
		}
		r, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			os.Exit(1)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
