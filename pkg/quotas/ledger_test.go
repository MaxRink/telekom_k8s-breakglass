// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
// SPDX-License-Identifier: Apache-2.0

package quotas

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testStore(t *testing.T) Store {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return Store{Client: c, Reader: c, Namespace: "controller"}
}
func noLegacy(context.Context, map[string]Entry) ([]Entry, error) { return nil, nil }
func alwaysLive(context.Context, Entry) (bool, error)             { return true, nil }
func claimant(uid string) Entry {
	return Entry{Kind: "Session", Namespace: "sessions", Name: uid, UID: uid, Scopes: []string{"global-user", "total"}}
}

func TestDurableReservationAcrossReplicasAndRestart(t *testing.T) {
	store := testStore(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	result := make(chan struct {
		entry Entry
		err   error
	}, 2)
	for _, uid := range []string{"one", "two"} {
		entry := claimant(uid)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := store.Reserve(t.Context(), entry, map[string]int32{"total": 1}, noLegacy, alwaysLive)
			result <- struct {
				entry Entry
				err   error
			}{entry, err}
		}()
	}
	close(start)
	wg.Wait()
	close(result)
	var winner, loser Entry
	for r := range result {
		if r.err == nil {
			require.Empty(t, winner.UID)
			winner = r.entry
		} else {
			require.ErrorIs(t, r.err, ErrFull)
			loser = r.entry
		}
	}
	require.NotEmpty(t, winner.UID)
	require.NotEmpty(t, loser.UID)
	// An unrelated worker/process retry cannot expire a committed reservation.
	restarted := Store{Client: store.Client, Reader: store.Reader, Namespace: store.Namespace}
	require.NoError(t, restarted.Reserve(t.Context(), winner, map[string]int32{"total": 0}, noLegacy, alwaysLive))
	require.ErrorIs(t, restarted.Reserve(t.Context(), loser, map[string]int32{"total": 1}, noLegacy, alwaysLive), ErrFull)
	live := func(_ context.Context, e Entry) (bool, error) { return e.UID != winner.UID, nil }
	require.NoError(t, restarted.Reserve(t.Context(), loser, map[string]int32{"total": 1}, noLegacy, live))
}

func TestReservationUsesAllScopesAndHeterogeneousLimits(t *testing.T) {
	store := testStore(t)
	for _, uid := range []string{"a", "b", "c"} {
		require.NoError(t, store.Reserve(t.Context(), claimant(uid), map[string]int32{"global-user": 3}, noLegacy, alwaysLive))
	}
	// Removing a low-order reservation does not let a stricter limit use a hole.
	live := func(_ context.Context, e Entry) (bool, error) { return e.UID != "a", nil }
	require.ErrorIs(t, store.Reserve(t.Context(), claimant("d"), map[string]int32{"global-user": 2, "total": 100}, noLegacy, live), ErrFull)
	require.NoError(t, store.Reserve(t.Context(), claimant("d"), map[string]int32{"global-user": 3, "total": 100}, noLegacy, live))
}

type failingCAS struct{ client.Client }

func (c failingCAS) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, obj.GetName())
}
func (c failingCAS) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("concurrent writer"))
}
func TestReservationCASExhaustionNeverSucceeds(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "already-exists", true: "conflict"}[exists], func(t *testing.T) {
			store := testStore(t)
			if exists {
				require.NoError(t, store.Reserve(t.Context(), claimant("seed"), map[string]int32{}, noLegacy, alwaysLive))
			}
			store.Client = failingCAS{Client: store.Client}
			err := store.Reserve(t.Context(), claimant("candidate"), map[string]int32{"total": 10}, noLegacy, alwaysLive)
			require.ErrorContains(t, err, "CAS retries exhausted")
		})
	}
}

func TestReservationNeverPrunesFromMissingOrStaleBootstrap(t *testing.T) {
	store := testStore(t)
	old := claimant("old")
	require.NoError(t, store.Reserve(t.Context(), old, map[string]int32{"total": 1}, noLegacy, alwaysLive))
	// An empty list does not prove deletion of an extant UID.
	require.ErrorIs(t, store.Reserve(t.Context(), claimant("new"), map[string]int32{"total": 1}, noLegacy, alwaysLive), ErrFull)
	// Conversely, a stale list cannot revive a UID proved terminal by live GET.
	stale := func(context.Context, map[string]Entry) ([]Entry, error) { return []Entry{old}, nil }
	live := func(_ context.Context, e Entry) (bool, error) { return e.UID != "old", nil }
	require.NoError(t, store.Reserve(t.Context(), claimant("new"), map[string]int32{"total": 1}, stale, live))
	// A name reuse cannot revive old reservation identity.
	reused := claimant("replacement")
	reused.Name = "new"
	require.ErrorIs(t, store.Reserve(t.Context(), reused, map[string]int32{"total": 1}, noLegacy, alwaysLive), ErrFull)
}

func TestReservationReadErrorsRetainCapacity(t *testing.T) {
	store := testStore(t)
	old := claimant("old")
	require.NoError(t, store.Reserve(t.Context(), old, map[string]int32{"total": 1}, noLegacy, alwaysLive))
	unavailable := func(context.Context, Entry) (bool, error) { return false, errors.New("API unavailable") }
	require.ErrorContains(t, store.Reserve(t.Context(), claimant("new"), map[string]int32{"total": 1}, noLegacy, unavailable), "API unavailable")
	require.ErrorIs(t, store.Reserve(t.Context(), claimant("new"), map[string]int32{"total": 1}, noLegacy, alwaysLive), ErrFull)
	require.Error(t, store.Reserve(t.Context(), Entry{}, nil, noLegacy, alwaysLive))
}
