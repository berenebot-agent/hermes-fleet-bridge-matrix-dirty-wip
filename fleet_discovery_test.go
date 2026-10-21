package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFleetDirectoryAdoption(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "owned"}[owned], func(t *testing.T) {
			e, ctx, _, _ := fleetFixture(t)
			dir := e.s.View().Members["iris"].Dir
			key := e.mx.secrets.APIKeys["iris"]
			e.mx.secrets.APIKeys = map[string]string{}
			id := "iris"
			if owned {
				id = "iris-123456abcdef"
				if err := atomicFile(filepath.Join(dir, ".fleet-owner.json"), jsonBytes(fleetOwner{Operation: "copy", Identity: id})); err != nil {
					t.Fatal(err)
				}
			}
			if err := atomicFile(filepath.Join(dir, ".fleet-availability.json"), []byte(`{"Paused":true}`)); err != nil {
				t.Fatal(err)
			}
			if err := atomicFile(filepath.Join(dir, "fleet-settings.json"), jsonBytes(fleetSettings{DefaultModel: "saved-model"})); err != nil {
				t.Fatal(err)
			}
			if _, err := configureEnv(filepath.Join(dir, "data", ".env"), "127.0.0.1", 9876); err != nil {
				t.Fatal(err)
			}
			if err := e.s.Update(func(st *State) error { st.Members = map[string]*Member{}; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(e.c.FleetDir, ".staging"), 0700); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := reconcileFleet(ctx, e.c, e.s, e.mx, e.docker); err != nil {
					t.Fatal(err)
				}
				m := e.s.View().Members[id]
				if m == nil || m.Name != "iris" || !m.Managed || !m.Paused || m.Port != 9876 || m.DefaultModel != "saved-model" || e.mx.key(id) != key {
					t.Fatalf("adoption did not preserve agent identity/configuration: %+v", m)
				}
				if len(e.s.View().Members) != 2 {
					t.Fatal("discovery duplicated agents or adopted staging directories")
				}
			}
		})
	}
}

func TestFleetDiscoveryFailsClosed(t *testing.T) {
	for _, kind := range []string{"symlink", "corrupt owner", "identity collision", "project collision", "port collision", "invalid compose", "retired identity"} {
		t.Run(kind, func(t *testing.T) {
			e, ctx, _, _ := fleetFixture(t)
			dir := filepath.Join(e.c.FleetDir, "nova")
			if kind == "symlink" {
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			} else {
				project := "nova"
				if kind == "project collision" {
					project = "iris"
				}
				writeFleetFixture(t, dir, project)
				port := 9876
				if kind == "port collision" {
					port = 8642
				}
				if _, err := configureEnv(filepath.Join(dir, "data", ".env"), "127.0.0.1", port); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "corrupt owner":
				if err := atomicFile(filepath.Join(dir, ".fleet-owner.json"), []byte(`{}`)); err != nil {
					t.Fatal(err)
				}
			case "identity collision", "retired identity":
				if kind == "retired identity" {
					if err := e.s.Update(func(st *State) error { st.Members["iris"].Deleted = true; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				if err := atomicFile(filepath.Join(dir, ".fleet-owner.json"), jsonBytes(fleetOwner{Operation: "copy", Identity: "iris"})); err != nil {
					t.Fatal(err)
				}
			case "invalid compose":
				if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := reconcileFleet(ctx, e.c, e.s, e.mx, e.docker); err == nil {
				t.Fatal("unsafe discovery accepted")
			}
			if fleetMember(e.s.View(), "nova") != nil {
				t.Fatal("unsafe agent enrolled")
			}
		})
	}
}

func TestFleetDirectoryPort(t *testing.T) {
	for _, text := range []string{"API_SERVER_PORT=9876\n", "export API_SERVER_PORT='9876'\n", "API_SERVER_PORT=9876 # saved\n", "OTHER='multi\nAPI_SERVER_PORT=1234\nline'\nAPI_SERVER_PORT=9876\n"} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "data"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data", ".env"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if port, err := fleetDirectoryPort(dir); err != nil || port != 9876 {
			t.Fatalf("port=%d error=%v", port, err)
		}
	}
}

func TestFleetDiscoveryLeavesPublishedOperationForRecovery(t *testing.T) {
	e, ctx, _, _ := fleetFixture(t)
	dir := filepath.Join(e.c.FleetDir, "nova")
	writeFleetFixture(t, dir, "nova")
	if err := e.s.Update(func(st *State) error {
		st.FleetOperation = &FleetOperation{Action: "copy", Name: "nova", Target: "nova-copy", Stage: "copying"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := reconcileFleet(ctx, e.c, e.s, e.mx, e.docker); err != nil {
		t.Fatal(err)
	}
	if fleetMember(e.s.View(), "nova") != nil {
		t.Fatal("discovery bypassed pending copy recovery")
	}
}

func TestFleetDirectoryRejectsInvalidPort(t *testing.T) {
	for _, text := range []string{"API_SERVER_PORT=invalid\n", "API_SERVER_PORT=80\n", "API_SERVER_PORT=9876\nAPI_SERVER_PORT=9877\n"} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "data"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data", ".env"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := fleetDirectoryPort(dir); err == nil {
			t.Fatal("invalid persisted API port accepted")
		}
	}
}

func TestFleetDiscoveryRejectsSymlinkedHomeBeforeReadingEnv(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(outside, ".env")
	if err := os.WriteFile(path, []byte("API_SERVER_PORT=9876\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "data")); err != nil {
		t.Fatal(err)
	}
	if _, err := fleetDirectoryPort(dir); err == nil {
		t.Fatal("symlinked Hermes home accepted")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("discovery touched configuration outside the fleet directory")
	}
}
