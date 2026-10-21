package main

// This module is loaded by the bundled Element Web through config.json "modules".
// It replaces the cryptoSetup extension so the client stops showing its encryption
// setup screen.
//
// The screensaver exists to protect encrypted history; this deployment keeps every
// room unencrypted (see spec §10), so there is nothing for it to set up. Element X
// is not affected: it ignores config.json and forces cross-signing on its own.
//
// Element's default extension leaves SHOW_ENCRYPTION_SETUP_UI undefined, and the
// client tests `0 == SHOW_ENCRYPTION_SETUP_UI`, which is false for undefined. A
// real boolean false makes that test pass, so postLoginSetup goes straight to the
// room list.
//
// shortcut: reaches into Element's internal _registerLegacyModule (the documented
// module API cannot set extensions). If a future Element removes it, the client
// falls back to its stock behaviour instead of breaking.
const elementModule = `class CryptoSetupExtension {
  constructor(api) {
    this.api = api;
    this.extensions = {
      cryptoSetup: {
        SHOW_ENCRYPTION_SETUP_UI: false,
        examineLoginResponse() {},
        persistCredentials() {},
        getSecretStorageKey() { return null; },
        createSecretStorageKey() { return null; },
        catchAccessSecretStorageError() {},
        setupEncryptionNeeded() { return false; },
        getDehydrationKeyCallback() { return null; },
      },
    };
  }
  async load() {
    const api = this.api || window.mxModuleApi;
    if (!api || typeof api._registerLegacyModule !== "function") {
      console.warn("fleet: Element module API unavailable; keeping the stock encryption setup screen");
      return;
    }
    api._registerLegacyModule(CryptoSetupExtension);
  }
}
CryptoSetupExtension.moduleApiVersion = ">=1.12.0";
export default CryptoSetupExtension;
`

const elementModulePath = "/modules/no-encryption-setup.js"
