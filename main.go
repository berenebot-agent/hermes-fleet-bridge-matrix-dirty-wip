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

func homeserverConfig(c Config) error {
	host, port, _ := net.SplitHostPort(c.MatrixListen)
	n, _ := strconv.Atoi(port)
	config := fmt.Sprintf("[global]\nserver_name=%q\ndatabase_path=%q\naddress=[%q]\nport=%d\nregistration_shared_secret_file=%q\nallow_registration=false\nallow_federation=false\nfederate_created_rooms=false\nallow_encryption=true\nallow_public_room_directory_without_auth=false\nlockdown_public_room_directory=true\nnew_user_displayname_suffix=\"\"\ndb_cache_capacity_mb=32.0\ndb_write_buffer_capacity_mb=16.0\ncache_capacity_modifier=0.1\nlog=\"error\"\nlog_colors=false\n", c.ServerName, filepath.Join(c.Data, "matrix"), host, n, filepath.Join(c.Data, "secrets", "registration"))
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
	defer mx.closeCrypto()
	d := newDocker()
	engine := &Engine{c: c, s: s, mx: mx, docker: d, agents: map[string]*agentRuntime{}}
	startup, stopStartup := context.WithTimeout(ctx, 2*time.Minute)
	err = reconcileFleet(startup, c, s, mx, d)
	stopStartup()
	if err != nil {
		return err
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
	server := &http.Server{Addr: c.Listen, Handler: engine.widgetRoutes(&ready, frontend(c, s, &ready)), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0)}
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
		if m.Enabled && !m.Deleted {
			if err = mx.provision(workCtx, s, id); err != nil {
				return fmt.Errorf("agent %s Matrix provisioning failed: %w", id, err)
			}
		}
	}
	engine.Start(workCtx)
	defer func() { ready.Store(false); cancel(); engine.wg.Wait() }()
	ready.Store(true)
	log.Printf("ready: %d fleet members; private Matrix identity locked", len(s.View().Members))
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
