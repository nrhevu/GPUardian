package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuardian/internal/model"
	"gpuardian/internal/protocol"
	runtimepkg "gpuardian/internal/runtime"
	"gpuardian/internal/store"
)

type listingDockerRuntime struct {
	daemonFakeRuntime
	containers []runtimepkg.DockerContainer
	calls      int
}

func (r *listingDockerRuntime) DockerContainers(context.Context) ([]runtimepkg.DockerContainer, error) {
	r.calls++
	return r.containers, nil
}

func syncManagedTestKey(t *testing.T, server *Server) (rootKey, tokenHash string) {
	t.Helper()
	rootKey, err := server.Store.ReadOrCreateRootKey()
	if err != nil {
		t.Fatal(err)
	}
	tokenHash = store.HashToken("gk_" + strings.Repeat("a", 48))
	snapshot := protocol.ManagedUserKeySnapshot{SnapshotID: "sha256:test", Keys: []protocol.ManagedUserKey{{ID: "uk_alice", Owner: "alice", Version: 1, Hash: tokenHash}}}
	response := serveNodeJSON(t, server, rootKey, http.MethodPut, "/api/v1/user-keys/sync", snapshot)
	if response.Code != http.StatusOK {
		t.Fatalf("sync status=%d body=%s", response.Code, response.Body.String())
	}
	return rootKey, tokenHash
}

func serveNodeJSON(t *testing.T, server *Server, rootKey, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(data)))
	req.Header.Set("Authorization", "Bearer "+rootKey)
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.nodeHTTPHandler().ServeHTTP(response, req)
	return response
}

func dockerContainerID(prefix string, i int) string {
	id := fmt.Sprintf("%s%04d", prefix, i)
	return id + strings.Repeat("0", 64-len(id))
}

func activeAuthorizationIDs(t *testing.T, server *Server, tokenHash string) map[string]bool {
	t.Helper()
	state, err := server.Store.EnforcementSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, authorization := range state.Authorizations {
		if authorization.TokenHash == tokenHash && authorization.Active && !authorization.Revoked {
			ids[authorization.ID] = true
		}
	}
	return ids
}

func TestManagedDockerAllowReclaimsRemovedContainersAtLimit(t *testing.T) {
	server := testServer(t)
	runtime := &listingDockerRuntime{}
	server.Runtime = runtime
	rootKey, tokenHash := syncManagedTestKey(t, server)

	old := time.Now().UTC().Add(-24 * time.Hour)
	for i := range 64 {
		authorization := model.Authorization{
			ID:        fmt.Sprintf("auth_%02d", i),
			Mode:      model.ModeDocker,
			TokenHash: tokenHash,
			TokenMode: model.TokenModeManaged,
			Holder:    "alice",
			CreatedAt: old.Add(time.Duration(i) * time.Minute),
			Active:    true,
		}
		switch {
		case i < 40:
			// Removed containers, oldest first.
			authorization.ContainerID = dockerContainerID("dead", i)
		case i < 60:
			authorization.ContainerID = dockerContainerID("live", i)
			runtime.containers = append(runtime.containers, runtimepkg.DockerContainer{ID: authorization.ContainerID})
		default:
			authorization.ContainerPattern = fmt.Sprintf("named-%d", i)
			runtime.containers = append(runtime.containers, runtimepkg.DockerContainer{ID: dockerContainerID("name", i), Names: []string{authorization.ContainerPattern}})
		}
		if err := server.Store.AddAuthorization(authorization); err != nil {
			t.Fatal(err)
		}
	}

	allow := protocol.AllowArgs{ID: "uk_alice", Mode: model.ModeDocker, Container: "new-job"}
	response := serveNodeJSON(t, server, rootKey, http.MethodPost, "/api/v1/allow", allow)
	if response.Code != http.StatusCreated {
		t.Fatalf("allow at limit status=%d body=%s", response.Code, response.Body.String())
	}
	active := activeAuthorizationIDs(t, server, tokenHash)
	if len(active) != 64-maxReclaimedDockerAuthorizations+1 {
		t.Fatalf("active authorizations = %d, want %d", len(active), 64-maxReclaimedDockerAuthorizations+1)
	}
	for i := range 64 {
		id := fmt.Sprintf("auth_%02d", i)
		if want := i >= maxReclaimedDockerAuthorizations; active[id] != want {
			t.Fatalf("%s active = %v, want %v", id, active[id], want)
		}
	}

	// Below the limit, an allow never lists containers.
	calls := runtime.calls
	allow.Container = "second-job"
	if response := serveNodeJSON(t, server, rootKey, http.MethodPost, "/api/v1/allow", allow); response.Code != http.StatusCreated {
		t.Fatalf("allow below limit status=%d body=%s", response.Code, response.Body.String())
	}
	if runtime.calls != calls {
		t.Fatalf("container listings = %d, want %d", runtime.calls, calls)
	}
}

func TestManagedDockerAllowKeepsLimitWhenContainersStillExist(t *testing.T) {
	server := testServer(t)
	runtime := &listingDockerRuntime{}
	server.Runtime = runtime
	rootKey, tokenHash := syncManagedTestKey(t, server)

	old := time.Now().UTC().Add(-24 * time.Hour)
	for i := range 64 {
		id := dockerContainerID("live", i)
		runtime.containers = append(runtime.containers, runtimepkg.DockerContainer{ID: id})
		if err := server.Store.AddAuthorization(model.Authorization{
			ID: fmt.Sprintf("auth_%02d", i), Mode: model.ModeDocker, TokenHash: tokenHash, TokenMode: model.TokenModeManaged,
			Holder: "alice", ContainerID: id, CreatedAt: old, Active: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	response := serveNodeJSON(t, server, rootKey, http.MethodPost, "/api/v1/allow", protocol.AllowArgs{ID: "uk_alice", Mode: model.ModeDocker, Container: "new-job"})
	if response.Code == http.StatusCreated || !strings.Contains(response.Body.String(), "active authorization limit") {
		t.Fatalf("allow with live containers status=%d body=%s", response.Code, response.Body.String())
	}
	if active := activeAuthorizationIDs(t, server, tokenHash); len(active) != 64 {
		t.Fatalf("active authorizations = %d, want 64", len(active))
	}
}

func TestReclaimableDockerAuthorizationsKeepLiveAndStandingScopes(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	dead := dockerContainerID("dead", 0)
	state := model.State{
		Authorizations: []model.Authorization{
			{ID: "removed_id", Mode: model.ModeDocker, TokenHash: "h", ContainerID: dead, CreatedAt: old, Active: true},
			{ID: "removed_name", Mode: model.ModeDocker, TokenHash: "h", ContainerPattern: "gone", CreatedAt: old.Add(time.Second), Active: true},
			{ID: "stopped", Mode: model.ModeDocker, TokenHash: "h", ContainerID: dockerContainerID("stop", 0), CreatedAt: old, Active: true},
			{ID: "renamed_alias", Mode: model.ModeDocker, TokenHash: "h", ContainerPattern: "alias", CreatedAt: old, Active: true},
			{ID: "wildcard", Mode: model.ModeDocker, TokenHash: "h", ContainerPattern: "job-*", CreatedAt: old, Active: true},
			{ID: "recent", Mode: model.ModeDocker, TokenHash: "h", ContainerPattern: "not-started", CreatedAt: now.Add(-time.Minute), Active: true},
			{ID: "claimed", Mode: model.ModeDocker, TokenHash: "h", ContainerID: dead, CreatedAt: old, Active: true},
			{ID: "revoked", Mode: model.ModeDocker, TokenHash: "h", ContainerID: dead, CreatedAt: old, Active: true, Revoked: true},
			{ID: "released", Mode: model.ModeDocker, TokenHash: "h", ContainerID: dead, CreatedAt: old},
			{ID: "other_token", Mode: model.ModeDocker, TokenHash: "other", ContainerID: dead, CreatedAt: old, Active: true},
			{ID: "user_mode", Mode: model.ModeUser, TokenHash: "h", Username: "alice", CreatedAt: old, Active: true},
		},
		SoftClaims: []model.SoftClaim{{ID: "claim", AuthorizationID: "claimed", TokenHash: "h"}},
	}
	containers := []runtimepkg.DockerContainer{
		{ID: dockerContainerID("stop", 0)},
		{ID: dockerContainerID("link", 0), Names: []string{"primary", "alias"}},
	}
	stale := staleDockerAuthorizations(reclaimableDockerAuthorizations(state, "h", now), containers)
	var got []string
	for _, authorization := range stale {
		got = append(got, authorization.ID)
	}
	if strings.Join(got, ",") != "removed_id,removed_name" {
		t.Fatalf("stale authorizations = %v, want [removed_id removed_name]", got)
	}
}
