package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFrontendUsesStockEncryptionSetup(t *testing.T) {
	c, s := testStore(t)
	var ready atomic.Bool
	ready.Store(true)
	h := frontend(c, s, &ready)
	for _, path := range []string{"/config.json", "/.well-known/matrix/client"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var config map[string]json.RawMessage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &config) != nil {
			t.Fatal("unreadable config", path)
		}
		for _, forbidden := range []string{"force_disable", "force_verification", "advancedEncryption", "no-encryption-setup", "modules"} {
			if strings.Contains(w.Body.String(), forbidden) {
				t.Fatal("encryption suppression remains", forbidden)
			}
		}
	}
}
