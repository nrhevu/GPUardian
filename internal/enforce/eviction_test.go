package enforce

import (
	"context"
	"testing"
	"time"

	"gpuardian/internal/model"
)

func TestEvictionReadsProcOnlyWhenAScopeCanBeStale(t *testing.T) {
	now := fixedNow().Add(time.Minute)
	tests := []struct {
		name      string
		mutate    func(*model.State)
		wantReads bool
	}{
		{name: "every scope valid", wantReads: false},
		{
			name:      "expired reservation",
			mutate:    func(state *model.State) { state.Reservations[0].ExpiresAt = fixedNow() },
			wantReads: true,
		},
		{
			name: "revoked claimed authorization",
			mutate: func(state *model.State) {
				state.Tokens = append(state.Tokens, model.Token{ID: "tok_claimed", Hash: "hash_claimed", Mode: model.TokenModeClaimed, ExpiresAt: fixedNow().Add(time.Hour)})
				state.Authorizations = append(state.Authorizations, authorization("auth_claimed", "hash_claimed", model.TokenModeClaimed, model.ModeUser, func(a *model.Authorization) {
					a.UID = 1000
					a.Revoked = true
				}))
			},
			wantReads: true,
		},
		{
			name: "expired legacy lease",
			mutate: func(state *model.State) {
				lease := activeLease(model.ModeUser, 1)
				lease.ExpiresAt = fixedNow()
				state.Leases = append(state.Leases, lease)
			},
			wantReads: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := model.State{
				Tokens:       []model.Token{token("hash_reserved", model.TokenModeReserved)},
				Reservations: []model.Reservation{reservation("hash_reserved", 0)},
				Authorizations: []model.Authorization{authorization("auth_valid", "hash_reserved", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
					a.UID = 1000
				})},
			}
			if tc.mutate != nil {
				tc.mutate(&state)
			}
			reader := &countingProc{
				infos:     map[int]model.ProcInfo{10: {PID: 10, StartTime: 10, UID: 1000}},
				infoCalls: make(map[int]int),
			}
			authz := Authorizer{Proc: reader, Killer: &fakeKiller{}, Now: func() time.Time { return now }}
			decisions, assessment, err := authz.EvictExpiredReservedProcesses(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
			if err != nil {
				t.Fatal(err)
			}
			if got := reader.infoCalls[10] > 0; got != tc.wantReads {
				t.Fatalf("proc reads = %d, want reads %v", reader.infoCalls[10], tc.wantReads)
			}
			if !tc.wantReads && (len(decisions) != 0 || len(assessment.Authorizations) != 0 || len(assessment.Leases) != 0) {
				t.Fatalf("early return changed the result: decisions=%+v assessment=%+v", decisions, assessment)
			}
		})
	}
}

func TestEvictionTreatsVanishedPIDAsGone(t *testing.T) {
	now := fixedNow().Add(time.Minute)
	expired := reservation("hash_old", 0)
	expired.ExpiresAt = fixedNow()
	state := model.State{
		Tokens:       []model.Token{token("hash_old", model.TokenModeReserved)},
		Reservations: []model.Reservation{expired},
		Authorizations: []model.Authorization{authorization("auth_old", "hash_old", model.TokenModeReserved, model.ModeUser, func(a *model.Authorization) {
			a.UID = 1000
			a.ExpiresAt = fixedNow()
		})},
	}
	authz := Authorizer{Proc: &liveProc{infos: map[int]model.ProcInfo{}}, Killer: &fakeKiller{}, Now: func() time.Time { return now }}
	_, assessment, err := authz.EvictExpiredReservedProcesses(context.Background(), state, []model.GPUProcess{gpuProcess(0, 10)})
	if err != nil {
		t.Fatalf("a PID that exited after the sample was reported as an inspection failure: %v", err)
	}
	if clean, ok := assessment.Authorizations["auth_old"]; !ok || !clean {
		t.Fatalf("assessment = %+v, want the stale scope assessed clean", assessment)
	}
}
