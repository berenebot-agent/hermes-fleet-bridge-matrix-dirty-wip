package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	_ "modernc.org/sqlite"
)

// Each bot has its own device and database: never share another bot's room keys.
type matrixCrypto struct {
	mu     sync.Mutex
	client *mautrix.Client
	helper *cryptohelper.CryptoHelper
	db     *dbutil.Database
}

var errNoEncryptionRecipient = errors.New("waiting for a cross-signed human device")

func (mx *Matrix) initCrypto(ctx context.Context, name string, bot Credential) error {
	mx.mu.Lock()
	defer mx.mu.Unlock()
	if mx.crypto[name] != nil {
		return nil
	}
	dir := filepath.Join(mx.c.Data, "secrets", "crypto", name)
	if err := secureDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := secureDir(dir); err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "pickle-key")
	key, err := readPrivate(keyPath)
	if os.IsNotExist(err) {
		if _, e := os.Stat(filepath.Join(dir, "crypto.db")); !os.IsNotExist(e) {
			return errors.New("crypto key missing; restore consistent backup")
		}
		key = []byte(randomSecret())
		err = atomicFile(keyPath, key)
	}
	if err != nil || len(key) < 32 {
		return errors.New("private crypto key unavailable")
	}
	if info, e := os.Lstat(filepath.Join(dir, "crypto.db")); e == nil && !info.Mode().IsRegular() {
		return errors.New("crypto database must be a regular file")
	} else if e != nil && !os.IsNotExist(e) {
		return errors.New("cannot inspect crypto database")
	}
	f, err := os.OpenFile(filepath.Join(dir, "crypto.db"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return errors.New("cannot open crypto database")
	}
	err = f.Chmod(0600)
	f.Close()
	if err != nil {
		return errors.New("cannot restrict crypto database")
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "crypto.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return errors.New("cannot open crypto database")
	}
	db.SetMaxOpenConns(1)
	wrapped, err := dbutil.NewWithDB(db, "sqlite3")
	if err != nil {
		db.Close()
		return errors.New("cannot initialise crypto database")
	}
	cli, err := mautrix.NewClient(mx.Base, id.UserID(bot.User), bot.Token)
	if err != nil {
		db.Close()
		return errors.New("cannot initialise encrypted Matrix client")
	}
	cli.DeviceID = "HERMES_FLEET_BRIDGE"
	cli.Log = zerolog.Nop() // Library errors may contain remote responses or key material.
	cli.Client = &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	helper, err := cryptohelper.NewCryptoHelper(cli, key, wrapped)
	if err == nil {
		err = helper.Init(ctx)
	}
	if err != nil {
		db.Close()
		return errors.New("Matrix crypto initialisation failed; preserve crypto storage and credentials")
	}
	cli.Crypto = helper
	mach := helper.Machine()
	// Normal Matrix TOFU: accept cross-signed devices, never blocked devices.
	mach.SendKeysMinTrust = id.TrustStateCrossSignedTOFU
	remote, err := cli.QueryKeys(ctx, &mautrix.ReqQueryKeys{DeviceKeys: mautrix.DeviceKeysRequest{cli.UserID: {cli.DeviceID}}})
	if err != nil {
		mach.Destroy()
		helper.Close()
		return errors.New("cannot check existing bot cryptographic identity")
	}
	seedPath := filepath.Join(dir, "cross-signing.json")
	seeds, err := readPrivate(seedPath)
	if os.IsNotExist(err) {
		if len(remote.MasterKeys[cli.UserID].Keys) > 0 {
			mach.Destroy()
			helper.Close()
			return errors.New("bot cross-signing secrets missing; restore consistent backup")
		}
		var keys *crypto.CrossSigningKeysCache
		keys, err = mach.GenerateCrossSigningKeys()
		if err == nil {
			seeds = jsonBytes(keys.Seeds())
			err = atomicFile(seedPath, seeds)
		}
	}
	var decoded crypto.CrossSigningSeeds
	if err == nil {
		err = json.Unmarshal(seeds, &decoded)
	}
	if err == nil {
		err = mach.ImportCrossSigningKeys(decoded)
	}
	if err == nil && len(remote.MasterKeys[cli.UserID].Keys) > 0 {
		master := mach.CrossSigningKeys.MasterKey.PublicKey()
		if remote.MasterKeys[cli.UserID].Keys[id.NewKeyID(id.KeyAlgorithmEd25519, master.String())] != master {
			err = errors.New("bot cryptographic identity mismatch")
		}
	}
	if err == nil {
		err = mach.PublishCrossSigningKeys(ctx, mach.CrossSigningKeys, func(uia *mautrix.RespUserInteractive) any {
			return &mautrix.ReqUIAuthLogin{BaseAuthData: mautrix.BaseAuthData{Type: mautrix.AuthTypePassword, Session: uia.Session}, User: bot.User, Password: bot.Password}
		})
	}
	if err == nil {
		err = mach.SignOwnDevice(ctx, mach.OwnIdentity())
	}
	if err != nil {
		mach.Destroy()
		helper.Close()
		return errors.New("bot cross-signing setup failed; private identity retained")
	}
	mx.crypto[name] = &matrixCrypto{client: cli, helper: helper, db: wrapped}
	return nil
}

func (mx *Matrix) cryptoFor(name string) *matrixCrypto {
	mx.mu.Lock()
	defer mx.mu.Unlock()
	return mx.crypto[name]
}

func (mx *Matrix) closeCrypto() {
	mx.mu.Lock()
	defer mx.mu.Unlock()
	for _, c := range mx.crypto {
		c.helper.Machine().Destroy()
		c.helper.Close()
	}
}

func (mx *Matrix) ensureEncrypted(ctx context.Context, name, room string) error {
	c := mx.cryptoFor(name)
	if c == nil {
		return errors.New("encrypted Matrix device unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var enc event.EncryptionEventContent
	err := c.client.StateEvent(ctx, id.RoomID(room), event.StateEncryption, "", &enc)
	if httpStatus(err, 404) || errors.Is(err, mautrix.MNotFound) {
		enc.Algorithm = id.AlgorithmMegolmV1
		_, err = c.client.SendStateEvent(ctx, id.RoomID(room), event.StateEncryption, "", &enc)
	}
	if err != nil || enc.Algorithm != id.AlgorithmMegolmV1 {
		return errors.New("room encryption unavailable; refusing plaintext fallback")
	}
	if err = c.client.StateStore.SetEncryptionEvent(ctx, id.RoomID(room), &enc); err != nil {
		return errors.New("cannot persist room encryption")
	}
	return nil
}

func (mx *Matrix) cryptoSync(ctx context.Context, name string, raw json.RawMessage, since string) error {
	if mx.crypto == nil {
		return nil
	} // Local transport fakes do not instantiate a crypto device.
	c := mx.cryptoFor(name)
	if c == nil {
		return errors.New("encrypted Matrix device unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var resp mautrix.RespSync
	if json.Unmarshal(raw, &resp) != nil {
		return errors.New("invalid encrypted sync response")
	}
	for room, joined := range resp.Rooms.Join {
		for _, ev := range append(joined.State.Events, joined.Timeline.Events...) {
			ev.RoomID = room
			if ev.StateKey != nil {
				ev.Type.Class = event.StateEventType
			}
			if ev.Type == event.StateMember || ev.Type == event.StateEncryption || ev.Type == event.StateHistoryVisibility {
				if err := ev.Content.ParseRaw(ev.Type); err != nil {
					return errors.New("invalid Matrix room state")
				}
				// State writes must succeed before the durable sync cursor advances.
				var err error
				switch ev.Type {
				case event.StateMember:
					err = c.client.StateStore.SetMember(ctx, room, id.UserID(ev.GetStateKey()), ev.Content.AsMember())
				case event.StateEncryption:
					err = c.client.StateStore.SetEncryptionEvent(ctx, room, ev.Content.AsEncryption())
				case event.StateHistoryVisibility:
					err = c.client.StateStore.SetHistoryVisibility(ctx, room, ev.Content.AsHistoryVisibility())
				}
				if err != nil {
					return errors.New("cannot persist crypto room state")
				}
				if ev.Type == event.StateMember {
					c.helper.Machine().HandleMemberEvent(ctx, ev)
				}
				if ev.Type == event.StateHistoryVisibility {
					c.helper.Machine().HandleHistoryVisibility(ctx, ev)
				}
			}
		}
	}
	c.helper.Machine().ProcessSyncResponse(ctx, &resp, since)
	return nil
}

func (mx *Matrix) decryptEvent(ctx context.Context, name, room string, raw json.RawMessage) (Event, error) {
	var result Event
	c := mx.cryptoFor(name)
	if c == nil {
		return result, errors.New("crypto unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var ev event.Event
	if json.Unmarshal(raw, &ev) != nil || ev.Content.ParseRaw(ev.Type) != nil {
		return result, errors.New("invalid encrypted event")
	}
	ev.RoomID = id.RoomID(room)
	decrypted, err := c.helper.Decrypt(ctx, &ev)
	if err != nil {
		if missingSession(err) {
			return result, crypto.ErrNoSessionFound
		}
		return result, errors.New("message keys unavailable")
	}
	if decrypted.Mautrix.TrustState < id.TrustStateCrossSignedTOFU {
		return result, errors.New("sender device is not cross-signed or trusted")
	}
	if decrypted.RoomID != ev.RoomID || decrypted.Sender != ev.Sender {
		return result, errors.New("encrypted event identity mismatch")
	}
	b, err := json.Marshal(decrypted)
	if err == nil {
		err = json.Unmarshal(b, &result)
	}
	result.Decrypted = true
	return result, err
}

func missingSession(err error) bool { return errors.Is(err, crypto.ErrNoSessionFound) }

func (mx *Matrix) requestEventKeys(ctx context.Context, name, room string, raw json.RawMessage) {
	c := mx.cryptoFor(name)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var ev event.Event
	if json.Unmarshal(raw, &ev) != nil || ev.Content.ParseRaw(ev.Type) != nil {
		return
	}
	enc := ev.Content.AsEncrypted()
	c.helper.RequestSession(ctx, id.RoomID(room), enc.SenderKey, enc.SessionID, ev.Sender, enc.DeviceID)
}

func (mx *Matrix) sendEncrypted(ctx context.Context, s *Store, name, room string, n *Notice, txn string, content any, result any) error {
	bot := mx.cred("agent_" + name)
	if mx.crypto == nil {
		return api(ctx, "PUT", mx.Base, roomPath(room)+"/send/m.room.message/"+txn, bot.Token, content, result, nil)
	}
	c := mx.cryptoFor(name)
	if c == nil {
		return errors.New("crypto unavailable; refusing plaintext fallback")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	wire := n.Ciphertext
	if len(wire) == 0 || string(wire) == "null" {
		members, err := c.client.Members(ctx, id.RoomID(room))
		if err != nil {
			return errors.New("cannot fetch room members for encryption")
		}
		if err = c.client.StateStore.ReplaceCachedMembers(ctx, id.RoomID(room), members.Chunk); err != nil {
			return errors.New("cannot persist encryption recipients")
		}
		var humans []id.UserID
		for _, member := range members.Chunk {
			if member.Content.AsMember().Membership == event.MembershipJoin && !botSender(member.GetStateKey()) {
				humans = append(humans, id.UserID(member.GetStateKey()))
			}
		}
		devices, err := c.helper.Machine().FetchKeys(ctx, humans, true)
		if err != nil {
			return errors.New("cannot fetch recipient encryption devices")
		}
		ready := false
		for _, user := range humans {
			for _, device := range devices[user] {
				trust, err := c.helper.Machine().ResolveTrustContext(ctx, device)
				if err != nil {
					return errors.New("cannot check recipient device trust")
				}
				if trust >= id.TrustStateCrossSignedTOFU {
					ready = true
				}
			}
		}
		if !ready {
			return errNoEncryptionRecipient
		}
		encrypted, err := c.helper.Encrypt(ctx, id.RoomID(room), event.EventMessage, content)
		if err != nil {
			return errors.New("cannot encrypt Matrix reply")
		}
		wire = jsonBytes(encrypted)
		if err = s.Update(func(st *State) error {
			i := noticeIndex(st.Members[name], n.ID)
			if i < 0 {
				return errors.New("reply no longer pending")
			}
			st.Members[name].Outbox[i].Ciphertext = wire
			return nil
		}); err != nil {
			return err
		}
	}
	return api(ctx, "PUT", mx.Base, roomPath(room)+"/send/m.room.encrypted/"+txn, bot.Token, wire, result, nil)
}
