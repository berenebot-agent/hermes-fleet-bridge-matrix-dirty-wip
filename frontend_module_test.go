package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The bundled Element Web only skips its "Confirm your identity" screen when the
// cryptoSetup extension reports SHOW_ENCRYPTION_SETUP_UI as a real boolean false.
// Test the served module actually carries that, so a silent regression cannot
// reintroduce the prompt without failing here.
func TestFrontendServesEncryptionSetupOverride(t *testing.T) {
	c, s := testStore(t)
	var ready atomic.Bool
	ready.Store(true)
	h := frontend(c, s, &ready)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/config.json", nil))
	var config struct {
		Modules []string `json:"modules"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &config) != nil {
		t.Fatal("config.json unreadable")
	}
	if len(config.Modules) != 1 || config.Modules[0] != elementModulePath {
		t.Fatalf("config.json does not load the Element module: %v", config.Modules)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", elementModulePath, nil))
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("module not served as JavaScript: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	for _, want := range []string{"SHOW_ENCRYPTION_SETUP_UI: false", "_registerLegacyModule", "export default"} {
		if !strings.Contains(body, want) {
			t.Fatalf("module missing %q", want)
		}
	}
	// The client compares `0 == SHOW_ENCRYPTION_SETUP_UI`, so the value must be an
	// actual boolean, never undefined-and-omitted.
	if strings.Contains(body, "SHOW_ENCRYPTION_SETUP_UI: undefined") {
		t.Fatal("module leaves the flag undefined; the client would still prompt")
	}
	// Element calls emit() on every registered module for each lifecycle event.
	// A module without it stops the client with "Your Element is misconfigured",
	// so the method has to survive every edit of the module source.
	if !strings.Contains(body, "emit() {}") {
		t.Fatal("module has no emit(); Element would fail to start")
	}
}

// Nothing is served before provisioning completes, including the module.
func TestFrontendHidesModuleUntilReady(t *testing.T) {
	c, s := testStore(t)
	var ready atomic.Bool
	h := frontend(c, s, &ready)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", elementModulePath, nil))
	if w.Code != 503 {
		t.Fatalf("module served before readiness: %d", w.Code)
	}
}
