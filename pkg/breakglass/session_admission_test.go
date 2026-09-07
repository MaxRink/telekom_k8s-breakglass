// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
// SPDX-License-Identifier: Apache-2.0

package breakglass

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	breakglassv1alpha1 "github.com/telekom/k8s-breakglass/api/v1alpha1"
	"github.com/telekom/k8s-breakglass/pkg/quotas"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSessionAdmissionGlobalScopesAndCrashRecovery(t *testing.T) {
	one := int32(1)
	for _, scope := range []string{"total", "user", "tuple"} {
		t.Run(scope, func(t *testing.T) {
			esc := &breakglassv1alpha1.BreakglassEscalation{ObjectMeta: metav1.ObjectMeta{Name: "escalation", Namespace: "one", UID: "escalation-one"}, Spec: breakglassv1alpha1.BreakglassEscalationSpec{SessionLimitsOverride: &breakglassv1alpha1.SessionLimitsOverride{}}}
			if scope == "total" {
				esc.Spec.SessionLimitsOverride.MaxActiveSessionsTotal = &one
			}
			if scope == "user" {
				esc.Spec.SessionLimitsOverride.MaxActiveSessionsPerUser = &one
			}
			secondEsc := esc.DeepCopy()
			secondEsc.Name = "second"
			secondEsc.Namespace = "two"
			secondEsc.UID = "escalation-two"
			candidate := func(name, namespace, user, group string, owner *breakglassv1alpha1.BreakglassEscalation) *breakglassv1alpha1.BreakglassSession {
				return &breakglassv1alpha1.BreakglassSession{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name), Annotations: map[string]string{quotas.AdmissionAnnotation: quotas.Pending}, OwnerReferences: []metav1.OwnerReference{{Kind: "BreakglassEscalation", Name: owner.Name, UID: owner.UID}}}, Spec: breakglassv1alpha1.BreakglassSessionSpec{User: user, Cluster: "cluster", GrantedGroup: group}}
			}
			a := candidate("first", "one", "user", "admin", esc)
			b := candidate("second", "two", "user", "other", secondEsc)
			if scope == "total" {
				b = candidate("second", "one", "different-user", "admin", esc)
			}
			if scope == "tuple" {
				b.Spec.GrantedGroup = "admin"
			}
			cli := fake.NewClientBuilder().WithScheme(Scheme).WithStatusSubresource(&breakglassv1alpha1.BreakglassSession{}).WithObjects(esc, secondEsc, a, b).Build()
			manager := NewSessionManagerWithClientAndReader(cli, cli, WithQuotaNamespace("controller"))
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(a), a))
			// Crash after committing reservation, before metadata/status publication.
			require.NoError(t, manager.reserveSession(t.Context(), a))
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(b), b))
			require.ErrorIs(t, manager.reserveSession(t.Context(), b), quotas.ErrFull)
			restarted := NewSessionManagerWithClientAndReader(cli, cli, WithQuotaNamespace("controller"))
			require.NoError(t, restarted.recoverSessionAdmissions(t.Context()))
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(a), a))
			assert.Equal(t, quotas.Ready, a.Annotations[quotas.AdmissionAnnotation])
			assert.Equal(t, breakglassv1alpha1.SessionStatePending, a.Status.State)
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(b), b))
			assert.Equal(t, breakglassv1alpha1.SessionStateRejected, b.Status.State)
		})
	}
}

func TestProvisionalSessionCannotGrantAccessOrApproval(t *testing.T) {
	for _, state := range []breakglassv1alpha1.BreakglassSessionState{breakglassv1alpha1.SessionStatePending, breakglassv1alpha1.SessionStateApproved} {
		s := breakglassv1alpha1.BreakglassSession{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{quotas.AdmissionAnnotation: quotas.Pending}}, Status: breakglassv1alpha1.BreakglassSessionStatus{State: state}}
		assert.False(t, IsSessionValid(s))
		assert.False(t, IsSessionPendingApproval(s))
		assert.False(t, IsSessionAccessActive(s))
		assert.False(t, isSessionTokenValid(s))
	}
}

func TestReservationRejectsTerminalOrRecreatedSession(t *testing.T) {
	s := &breakglassv1alpha1.BreakglassSession{ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "ns", UID: "new"}, Status: breakglassv1alpha1.BreakglassSessionStatus{State: breakglassv1alpha1.SessionStateRejected}}
	cli := fake.NewClientBuilder().WithScheme(Scheme).WithStatusSubresource(&breakglassv1alpha1.BreakglassSession{}).WithObjects(s).Build()
	sm := NewSessionManagerWithClientAndReader(cli, cli, WithQuotaNamespace("controller"))
	desired := *s
	desired.UID = "old"
	desired.Status.State = breakglassv1alpha1.SessionStateApproved
	require.Error(t, sm.UpdateBreakglassSessionStatus(context.Background(), desired))
	current := &breakglassv1alpha1.BreakglassSession{}
	require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(s), current))
	assert.Equal(t, breakglassv1alpha1.SessionStateRejected, current.Status.State)
}

type failInitialQuotaStatusClient struct {
	client.Client
	fail bool
}
type failInitialQuotaStatusWriter struct {
	client.SubResourceWriter
	owner *failInitialQuotaStatusClient
}

func (c *failInitialQuotaStatusClient) Status() client.SubResourceWriter {
	return failInitialQuotaStatusWriter{SubResourceWriter: c.Client.Status(), owner: c}
}
func (w failInitialQuotaStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if w.owner.fail {
		w.owner.fail = false
		return fmt.Errorf("injected initial status failure")
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestQuotaReservationSurvivesInitialStatusFailure(t *testing.T) {
	esc := &breakglassv1alpha1.BreakglassEscalation{ObjectMeta: metav1.ObjectMeta{Name: "escalation", Namespace: "ns", UID: "esc"}}
	s := &breakglassv1alpha1.BreakglassSession{ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "ns", UID: "session", Annotations: map[string]string{quotas.AdmissionAnnotation: quotas.Pending}, OwnerReferences: []metav1.OwnerReference{{Kind: "BreakglassEscalation", Name: esc.Name, UID: esc.UID}}}, Spec: breakglassv1alpha1.BreakglassSessionSpec{User: "user", Cluster: "cluster", GrantedGroup: "admin"}}
	cli := fake.NewClientBuilder().WithScheme(Scheme).WithStatusSubresource(&breakglassv1alpha1.BreakglassSession{}).WithObjects(esc, s).Build()
	faulty := &failInitialQuotaStatusClient{Client: cli, fail: true}
	sm := NewSessionManagerWithClientAndReader(faulty, cli, WithQuotaNamespace("controller"))
	require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(s), s))
	require.NoError(t, sm.admitSession(t.Context(), s))
	s.Status.State = breakglassv1alpha1.SessionStatePending
	require.ErrorContains(t, sm.UpdateBreakglassSessionStatus(t.Context(), *s), "injected initial status failure")
	require.NoError(t, sm.recoverSessionAdmissions(t.Context()))
	require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(s), s))
	assert.Equal(t, breakglassv1alpha1.SessionStatePending, s.Status.State)
	assert.Equal(t, quotas.Ready, s.Annotations[quotas.AdmissionAnnotation])
}

func TestDurableQuotaLimitPrecedence(t *testing.T) {
	one, two, three := int32(1), int32(2), int32(3)
	idp := &breakglassv1alpha1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "idp"}, Spec: breakglassv1alpha1.IdentityProviderSpec{SessionLimits: &breakglassv1alpha1.SessionLimits{MaxActiveSessionsPerUser: &one, GroupOverrides: []breakglassv1alpha1.SessionLimitGroupOverride{{Group: "platform-*", MaxActiveSessionsPerUser: &two}}}}}
	cli := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(idp).Build()
	sm := NewSessionManagerWithClientAndReader(cli, cli, WithQuotaNamespace("controller"))
	session := &breakglassv1alpha1.BreakglassSession{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"breakglass.t-caas.telekom.com/quota-user-groups": `["platform-team"]`}}, Spec: breakglassv1alpha1.BreakglassSessionSpec{User: "user", Cluster: "cluster", GrantedGroup: "admin", IdentityProviderName: idp.Name}}
	esc := &breakglassv1alpha1.BreakglassEscalation{ObjectMeta: metav1.ObjectMeta{UID: "esc"}, Spec: breakglassv1alpha1.BreakglassEscalationSpec{SessionLimitsOverride: &breakglassv1alpha1.SessionLimitsOverride{MaxActiveSessionsTotal: &three}}}
	limits, err := sm.sessionQuotaLimits(t.Context(), session, esc)
	require.NoError(t, err)
	assert.Equal(t, two, limits[sessionScope("user", "user")])
	assert.Equal(t, three, limits[sessionScope("escalation", "esc")])
	esc.Spec.SessionLimitsOverride.MaxActiveSessionsPerUser = &three
	limits, err = sm.sessionQuotaLimits(t.Context(), session, esc)
	require.NoError(t, err)
	assert.Equal(t, three, limits[sessionScope("user", "user")])
	esc.Spec.SessionLimitsOverride.Unlimited = true
	limits, err = sm.sessionQuotaLimits(t.Context(), session, esc)
	require.NoError(t, err)
	assert.Equal(t, map[string]int32{sessionScope("tuple", "user", "cluster", "admin"): 1}, limits)
}
