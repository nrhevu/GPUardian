package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gpuardian/internal/config"
	"gpuardian/internal/model"
	"gpuardian/internal/protocol"
	"gpuardian/internal/telemetry"
)

type authzNodeClient struct {
	mu                sync.Mutex
	snapshot          model.NodeSnapshot
	keys              model.KeyStatus
	lastReservation   protocol.RegisterArgs
	lastClaim         protocol.RegisterArgs
	reservationResult model.RegisterResult
	allowed           []protocol.AllowArgs
	revoked           []string
}

type managedAuthzNodeClient struct {
	*authzNodeClient
	snapshot protocol.ManagedUserKeySnapshot
}

func (c *managedAuthzNodeClient) Info(context.Context, ServerRecord) (telemetry.Info, error) {
	return telemetry.Info{NodeID: "node-managed", Capabilities: []string{"managed_user_keys_v1"}}, nil
}

func (c *managedAuthzNodeClient) Telemetry(context.Context, ServerRecord, string, int) (telemetry.Page, error) {
	return telemetry.Page{}, nil
}

func (c *managedAuthzNodeClient) SyncManagedUserKeys(_ context.Context, _ ServerRecord, snapshot protocol.ManagedUserKeySnapshot) (protocol.ManagedUserKeySyncResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = snapshot
	c.keys.Tokens = nil
	for _, key := range snapshot.Keys {
		c.keys.Tokens = append(c.keys.Tokens, model.TokenView{ID: key.ID, Name: key.Owner, Mode: model.TokenModeManaged, Version: key.Version, Managed: true})
	}
	return protocol.ManagedUserKeySyncResult{SnapshotID: snapshot.SnapshotID, Applied: len(snapshot.Keys), Managed: true}, nil
}

func (c *authzNodeClient) Health(context.Context, ServerRecord, string) error {
	return nil
}

func (c *authzNodeClient) Snapshot(context.Context, ServerRecord) (model.NodeSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot, nil
}

func (c *authzNodeClient) CreateReservation(_ context.Context, _ ServerRecord, args protocol.RegisterArgs) (model.RegisterResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastReservation = args
	if c.reservationResult.Mode != "" {
		return c.reservationResult, nil
	}
	return model.RegisterResult{Token: "rg_reserved", TokenID: "tok_reserved", Mode: model.TokenModeReserved}, nil
}

func (c *authzNodeClient) CreateClaimKey(_ context.Context, _ ServerRecord, args protocol.RegisterArgs) (model.RegisterResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastClaim = args
	return model.RegisterResult{Token: "rg_claim"}, nil
}

func (c *authzNodeClient) ShowKeys(context.Context, ServerRecord, string) (model.KeyStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keys, nil
}

func (c *authzNodeClient) Allow(_ context.Context, _ ServerRecord, args protocol.AllowArgs) (model.AllowResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.allowed = append(c.allowed, args)
	return model.AllowResult{AuthorizationID: "auth_" + args.ID, Mode: args.Mode}, nil
}

func (c *authzNodeClient) Revoke(_ context.Context, _ ServerRecord, args protocol.RevokeArgs) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked = append(c.revoked, args.ID)
	return map[string]string{"revoked": args.ID}, nil
}

func TestGatewayRoleAuthorization(t *testing.T) {
	server, client, serverID := newAuthzServer(t)
	handler := server.routes()
	userCookie := testSessionCookie(t, server, "alice", RoleUser)
	adminCookie := testSessionCookie(t, server, "admin", RoleAdmin)

	userAdd := requestJSON(handler, http.MethodPost, "/api/servers", `{"name":"n","endpoint":"https://node","root_key":"rk"}`, userCookie)
	if userAdd.Code != http.StatusForbidden {
		t.Fatalf("user add server status = %d, body=%s", userAdd.Code, userAdd.Body.String())
	}

	userCreate := requestJSON(handler, http.MethodPost, "/api/users", `{"username":"bob","password":"secret"}`, userCookie)
	if userCreate.Code != http.StatusForbidden {
		t.Fatalf("user create user status = %d, body=%s", userCreate.Code, userCreate.Body.String())
	}

	adminCreate := requestJSON(handler, http.MethodPost, "/api/users", `{"username":"bob","password":"test-password-strong"}`, adminCookie)
	if adminCreate.Code != http.StatusCreated {
		t.Fatalf("admin create user status = %d, body=%s", adminCreate.Code, adminCreate.Body.String())
	}

	userDelete := requestJSON(handler, http.MethodDelete, "/api/users", `{"username":"bob"}`, userCookie)
	if userDelete.Code != http.StatusForbidden {
		t.Fatalf("user delete status = %d, body=%s", userDelete.Code, userDelete.Body.String())
	}

	selfDelete := requestJSON(handler, http.MethodDelete, "/api/users", `{"username":"admin"}`, adminCookie)
	if selfDelete.Code != http.StatusBadRequest {
		t.Fatalf("admin self-delete status = %d, body=%s", selfDelete.Code, selfDelete.Body.String())
	}

	adminDelete := requestJSON(handler, http.MethodDelete, "/api/users", `{"username":"bob"}`, adminCookie)
	if adminDelete.Code != http.StatusOK {
		t.Fatalf("admin delete user status = %d, body=%s", adminDelete.Code, adminDelete.Body.String())
	}
	if _, found, err := server.Users.Get("bob"); err != nil || found {
		t.Fatalf("deleted user found=%v, err=%v", found, err)
	}

	client.mu.Lock()
	client.reservationResult = model.RegisterResult{
		Token:          "rg_reserved",
		Mode:           model.TokenModeReserved,
		ReservationIDs: []string{"res_reserved"},
	}
	client.keys.Tokens = append(client.keys.Tokens, model.TokenView{
		ID: "tok_reserved", Key: "rg_reserved", Name: "alice", Mode: model.TokenModeReserved,
	})
	client.keys.Reservations = append(client.keys.Reservations, model.ReservationView{
		ID: "res_reserved", GroupID: "tok_reserved", GPU: 0, Holder: "alice", Active: true,
	})
	client.mu.Unlock()
	reserve := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/reservations", `{"name":"mallory","purpose":"test","gpus":[0]}`, userCookie)
	if reserve.Code != http.StatusCreated {
		t.Fatalf("reserve status = %d, body=%s", reserve.Code, reserve.Body.String())
	}
	var reserveResult model.RegisterResult
	if err := json.Unmarshal(reserve.Body.Bytes(), &reserveResult); err != nil {
		t.Fatal(err)
	}
	if reserveResult.Token != "" || reserveResult.TokenID != "" {
		t.Fatalf("reserve result leaked credential identity: %+v", reserveResult)
	}
	client.mu.Lock()
	gotOwner := client.lastReservation.Name
	client.mu.Unlock()
	if gotOwner != "alice" {
		t.Fatalf("reservation owner = %q, want alice", gotOwner)
	}
}

func TestGatewayFiltersAndAuthorizesOwnedKeys(t *testing.T) {
	server, client, serverID := newAuthzServer(t)
	handler := server.routes()
	userCookie := testSessionCookie(t, server, "alice", RoleUser)
	adminCookie := testSessionCookie(t, server, "admin", RoleAdmin)

	snapshot := requestJSON(handler, http.MethodGet, "/api/fleet/snapshot", "", userCookie)
	if snapshot.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d, body=%s", snapshot.Code, snapshot.Body.String())
	}
	var fleet fleetSnapshot
	if err := json.Unmarshal(snapshot.Body.Bytes(), &fleet); err != nil {
		t.Fatal(err)
	}
	tokens := fleet.Servers[0].Snapshot.Tokens
	if len(tokens) != 1 || tokens[0].Name != "alice" {
		t.Fatalf("filtered tokens = %+v, want only alice token", tokens)
	}
	if got := len(fleet.Servers[0].Snapshot.Reservations); got != 2 {
		t.Fatalf("visible reservations = %d, want 2", got)
	}

	ownKey := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/show-key", `{"id":"tok_alice"}`, userCookie)
	if ownKey.Code != http.StatusOK || !bytes.Contains(ownKey.Body.Bytes(), []byte("rg_alice")) {
		t.Fatalf("own show-key = %d %s", ownKey.Code, ownKey.Body.String())
	}

	otherKey := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/show-key", `{"id":"tok_bob"}`, userCookie)
	if otherKey.Code != http.StatusForbidden {
		t.Fatalf("other show-key = %d, body=%s", otherKey.Code, otherKey.Body.String())
	}

	otherRevoke := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/revoke", `{"id":"tok_bob"}`, userCookie)
	if otherRevoke.Code != http.StatusForbidden {
		t.Fatalf("other revoke = %d, body=%s", otherRevoke.Code, otherRevoke.Body.String())
	}

	otherAllow := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/allow", `{"id":"tok_bob","mode":"user","user":"alice"}`, userCookie)
	if otherAllow.Code != http.StatusForbidden {
		t.Fatalf("other allow = %d, body=%s", otherAllow.Code, otherAllow.Body.String())
	}

	ownAllow := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/allow", `{"id":"tok_alice","mode":"docker","container":"trainer"}`, userCookie)
	if ownAllow.Code != http.StatusCreated {
		t.Fatalf("own allow = %d, body=%s", ownAllow.Code, ownAllow.Body.String())
	}

	otherRuleRevoke := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/revoke", `{"id":"auth_bob"}`, userCookie)
	if otherRuleRevoke.Code != http.StatusForbidden {
		t.Fatalf("other rule revoke = %d, body=%s", otherRuleRevoke.Code, otherRuleRevoke.Body.String())
	}

	ownRuleRevoke := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/revoke", `{"id":"auth_alice"}`, userCookie)
	if ownRuleRevoke.Code != http.StatusOK {
		t.Fatalf("own rule revoke = %d, body=%s", ownRuleRevoke.Code, ownRuleRevoke.Body.String())
	}

	ownRevoke := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/revoke", `{"id":"tok_alice"}`, userCookie)
	if ownRevoke.Code != http.StatusOK {
		t.Fatalf("own revoke = %d, body=%s", ownRevoke.Code, ownRevoke.Body.String())
	}

	adminRevoke := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/revoke", `{"id":"tok_bob"}`, adminCookie)
	if adminRevoke.Code != http.StatusOK {
		t.Fatalf("admin revoke = %d, body=%s", adminRevoke.Code, adminRevoke.Body.String())
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.revoked) != 3 || client.revoked[0] != "auth_alice" || client.revoked[1] != "tok_alice" || client.revoked[2] != "tok_bob" {
		t.Fatalf("revoked ids = %+v, want auth_alice, tok_alice, then tok_bob", client.revoked)
	}
	if len(client.allowed) != 1 || client.allowed[0].ID != "tok_alice" || client.allowed[0].Container != "trainer" {
		t.Fatalf("allowed args = %+v, want tok_alice docker trainer", client.allowed)
	}
}

func TestFixedKeyAPIRestrictsRevealAndRegenerate(t *testing.T) {
	server, _, _ := newAuthzServer(t)
	if err := server.Users.InitializeFixedKeys(bytes.Repeat([]byte{0x33}, 32)); err != nil {
		t.Fatal(err)
	}
	handler := server.routes()
	aliceCookie := testSessionCookie(t, server, "alice", RoleUser)
	adminCookie := testSessionCookie(t, server, "admin", RoleAdmin)

	list := requestJSON(handler, http.MethodGet, "/api/keys", "", aliceCookie)
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var keys []FixedUserKey
	if err := json.Unmarshal(list.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Owner != "alice" || keys[0].Secret != "" {
		t.Fatalf("user list leaked or returned another key: %+v", keys)
	}
	if response := requestJSON(handler, http.MethodPost, "/api/keys/admin/reveal", "", aliceCookie); response.Code != http.StatusForbidden {
		t.Fatalf("user revealed another key: %d %s", response.Code, response.Body.String())
	}
	reveal := requestJSON(handler, http.MethodPost, "/api/keys/alice/reveal", "", aliceCookie)
	if reveal.Code != http.StatusOK {
		t.Fatalf("reveal status=%d body=%s", reveal.Code, reveal.Body.String())
	}
	var first FixedUserKey
	if err := json.Unmarshal(reveal.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	regenerate := requestJSON(handler, http.MethodPost, "/api/keys/alice/regenerate", "", adminCookie)
	if regenerate.Code != http.StatusOK {
		t.Fatalf("admin regenerate status=%d body=%s", regenerate.Code, regenerate.Body.String())
	}
	var replacement FixedUserKey
	if err := json.Unmarshal(regenerate.Body.Bytes(), &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.Version != first.Version+1 || replacement.ID == first.ID || replacement.Secret == first.Secret {
		t.Fatalf("key was not replaced: before=%+v after=%+v", first, replacement)
	}
	stillAuthenticated := requestJSON(handler, http.MethodGet, "/api/keys", "", aliceCookie)
	if stillAuthenticated.Code != http.StatusOK {
		t.Fatalf("key regeneration invalidated the owner session: %d %s", stillAuthenticated.Code, stillAuthenticated.Body.String())
	}
}

func TestManagedGatewayIgnoresClientKeyIdentity(t *testing.T) {
	server, legacy, serverID := newAuthzServer(t)
	if err := server.Users.InitializeFixedKeys(bytes.Repeat([]byte{0x66}, 32)); err != nil {
		t.Fatal(err)
	}
	aliceKey, err := server.Users.FixedKeyForUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	managed := &managedAuthzNodeClient{authzNodeClient: legacy}
	server.Client = managed
	legacy.reservationResult = model.RegisterResult{Token: "must-not-leak", TokenID: aliceKey.ID, GroupID: "grp_test", Mode: model.TokenModeManaged, ReservationIDs: []string{"res_test"}, GPUs: []int{0}}
	handler := server.routes()
	cookie := testSessionCookie(t, server, "alice", RoleUser)
	reserve := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/reservations", `{"name":"mallory","user_key_id":"uk_attacker","gpus":[0]}`, cookie)
	if reserve.Code != http.StatusCreated {
		t.Fatalf("reserve status=%d body=%s", reserve.Code, reserve.Body.String())
	}
	var result model.RegisterResult
	if err := json.Unmarshal(reserve.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Token != "" || result.GroupID != "grp_test" {
		t.Fatalf("reservation response leaked secret or lost group: %+v", result)
	}
	legacy.mu.Lock()
	gotReservation := legacy.lastReservation
	legacy.mu.Unlock()
	if gotReservation.Name != "alice" || gotReservation.UserKeyID != aliceKey.ID {
		t.Fatalf("client selected managed identity: %+v", gotReservation)
	}

	allow := requestJSON(handler, http.MethodPost, "/api/servers/"+serverID+"/allow", `{"id":"uk_attacker","user_key_id":"uk_attacker","mode":"user","user":"alice"}`, cookie)
	if allow.Code != http.StatusCreated {
		t.Fatalf("allow status=%d body=%s", allow.Code, allow.Body.String())
	}
	legacy.mu.Lock()
	defer legacy.mu.Unlock()
	if len(legacy.allowed) != 1 || legacy.allowed[0].ID != aliceKey.ID || legacy.allowed[0].UserKeyID != aliceKey.ID {
		t.Fatalf("client selected authorization identity: %+v", legacy.allowed)
	}
}

func newAuthzServer(t *testing.T) (*Server, *authzNodeClient, string) {
	t.Helper()
	tmp := t.TempDir()
	server := New(config.Config{
		WebUser:     "admin",
		WebPassword: "test-password-strong",
		WebRegistry: filepath.Join(tmp, "servers.json"),
		WebUsers:    filepath.Join(tmp, "users.json"),
	})
	if err := server.Users.BootstrapAdmin("admin", "test-password-strong"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Users.Create("alice", "test-password-strong", RoleUser); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0).UTC()
	client := &authzNodeClient{
		snapshot: model.NodeSnapshot{
			Now: now,
			Tokens: []model.TokenView{
				{ID: "tok_alice", Name: "alice", Mode: model.TokenModeReserved, CreatedAt: now},
				{ID: "tok_bob", Name: "bob", Mode: model.TokenModeReserved, CreatedAt: now},
			},
			Reservations: []model.ReservationView{
				{ID: "res_alice", GroupID: "tok_alice", GPU: 0, Holder: "alice", CreatedAt: now, StartsAt: now, ExpiresAt: now.Add(time.Hour), Active: true},
				{ID: "res_bob", GroupID: "tok_bob", GPU: 1, Holder: "bob", CreatedAt: now, StartsAt: now, ExpiresAt: now.Add(time.Hour), Active: true},
			},
		},
		keys: model.KeyStatus{
			Now: now,
			Tokens: []model.TokenView{
				{ID: "tok_alice", Key: "rg_alice", Name: "alice", Mode: model.TokenModeReserved, CreatedAt: now},
				{ID: "tok_bob", Key: "rg_bob", Name: "bob", Mode: model.TokenModeReserved, CreatedAt: now},
			},
			Reservations: []model.ReservationView{
				{ID: "res_alice", GroupID: "tok_alice", GPU: 0, Holder: "alice", CreatedAt: now, StartsAt: now, ExpiresAt: now.Add(time.Hour), Active: true},
				{ID: "res_bob", GroupID: "tok_bob", GPU: 1, Holder: "bob", CreatedAt: now, StartsAt: now, ExpiresAt: now.Add(time.Hour), Active: true},
			},
			Authorizations: []model.AuthorizationView{
				{ID: "auth_alice", TokenID: "tok_alice", Mode: model.ModeDocker, Holder: "alice", ContainerPattern: "trainer", CreatedAt: now, Active: true},
				{ID: "auth_bob", TokenID: "tok_bob", Mode: model.ModeUser, Holder: "bob", Username: "bob", CreatedAt: now, Active: true},
			},
		},
	}
	server.Client = client
	stored, err := server.Registry.Upsert(ServerRecord{
		Name:     "node-a",
		Endpoint: "https://node-a:8443",
		RootKey:  "rk_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return server, client, stored.ID
}

func testSessionCookie(t *testing.T, server *Server, username, role string) *http.Cookie {
	t.Helper()
	expires := time.Now().Add(time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	return server.sessionCookie(req, server.signSession(username, role, expires), expires)
}

func requestJSON(handler http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}
