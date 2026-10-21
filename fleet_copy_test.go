package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A real Hermes home contains a socket, a relative symlink, an absolute
// symlink into the agent image and hardlinked files. The copy must reproduce
// all of it, skipping only the socket.
func TestCopyFleetTreeReproducesRealHome(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	for _, d := range []string{src, dst} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// A stopped agent leaves a live socket behind; it is runtime state.
	if err := makeUnixSocket(filepath.Join(src, "gateway.sock")); err != nil {
		t.Skipf("socket unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "state.db"), []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	// A virtualenv: relative siblings plus an absolute interpreter pointer.
	venv := filepath.Join(src, "installs", "env", "venv", "bin")
	if err := os.MkdirAll(venv, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "python"), []byte("bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("python", filepath.Join(venv, "python3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/hermes/tools/python/bin/python3", filepath.Join(venv, "python3.14")); err != nil {
		t.Fatal(err)
	}
	// A hardlinked pair stays shared in source; the copy may split it, but it
	// must copy both paths.
	a, b := filepath.Join(src, "cache", "a"), filepath.Join(src, "cache", "b")
	if err := os.MkdirAll(filepath.Dir(a), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a, []byte("shared"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, b); err != nil {
		t.Fatal(err)
	}

	if err := copyFleetTree(context.Background(), src, dst); err != nil {
		t.Fatalf("copy refused a real Hermes home: %v", err)
	}

	if b, err := os.ReadFile(filepath.Join(dst, "state.db")); err != nil || string(b) != "state" {
		t.Fatal("regular file not copied")
	}
	if _, err := os.Lstat(filepath.Join(dst, "gateway.sock")); !os.IsNotExist(err) {
		t.Fatal("socket should be skipped, not copied")
	}
	if link, err := os.Readlink(filepath.Join(dst, "installs", "env", "venv", "bin", "python3")); err != nil || link != "python" {
		t.Fatalf("relative symlink not preserved: %q %v", link, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "installs", "env", "venv", "bin", "python3.14")); err != nil {
		t.Fatal("absolute symlink not copied")
	}
	for _, name := range []string{"a", "b"} {
		if b, err := os.ReadFile(filepath.Join(dst, "cache", name)); err != nil || string(b) != "shared" {
			t.Fatalf("hardlinked path %s not copied", name)
		}
	}
	if info, err := os.Stat(filepath.Join(dst, "state.db")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("file mode not preserved")
	}
}

// An absolute symlink pointing inside the copied tree must follow the copy.
func TestCopyFleetTreeRelocatesInternalSymlink(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	for _, d := range []string{src, dst} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "real"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "real"), filepath.Join(src, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := copyFleetTree(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(dst, "alias"))
	if err != nil {
		t.Fatal(err)
	}
	if link != filepath.Join(dst, "real") {
		t.Fatalf("internal symlink not relocated: %q", link)
	}
}

// The ownership record of the source must never leak into the copy.
func TestCopyFleetTreeSkipsBridgeMetadata(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	for _, d := range []string{src, dst} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".fleet-owner.json", "fleet-template.json"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyFleetTree(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".fleet-owner.json", "fleet-template.json"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); !os.IsNotExist(err) {
			t.Fatalf("%s copied into the destination", name)
		}
	}
}

func makeUnixSocket(path string) error {
	f, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	f.SetUnlinkOnClose(false)
	return f.Close()
}
