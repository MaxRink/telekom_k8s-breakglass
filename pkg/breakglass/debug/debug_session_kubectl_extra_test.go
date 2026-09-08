/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package debug

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	breakglassv1alpha1 "github.com/telekom/k8s-breakglass/api/v1alpha1"
)

func TestFindActiveSession(t *testing.T) {
	scheme := newKubectlTestScheme()

	activeSession := &breakglassv1alpha1.DebugSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "active-session",
			Namespace: "default",
		},
		Spec: breakglassv1alpha1.DebugSessionSpec{
			Cluster:     "test-cluster",
			RequestedBy: "user@example.com",
		},
		Status: breakglassv1alpha1.DebugSessionStatus{
			State: breakglassv1alpha1.DebugSessionStateActive,
			Participants: []breakglassv1alpha1.DebugSessionParticipant{
				{User: "user@example.com", Role: breakglassv1alpha1.ParticipantRoleParticipant, IdentityProviderIssuer: "https://test-idp.example"},
			},
		},
	}

	otherSession := &breakglassv1alpha1.DebugSession{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default"},
		Spec:       breakglassv1alpha1.DebugSessionSpec{Cluster: "other-cluster"},
		Status:     breakglassv1alpha1.DebugSessionStatus{State: breakglassv1alpha1.DebugSessionStateActive},
	}

	expiredSession := &breakglassv1alpha1.DebugSession{
		ObjectMeta: metav1.ObjectMeta{Name: "expired", Namespace: "default"},
		Spec:       breakglassv1alpha1.DebugSessionSpec{Cluster: "test-cluster", RequestedBy: "user@example.com"},
		Status: breakglassv1alpha1.DebugSessionStatus{
			State:     breakglassv1alpha1.DebugSessionStateActive,
			ExpiresAt: &metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
			Participants: []breakglassv1alpha1.DebugSessionParticipant{
				{User: "user@example.com", Role: breakglassv1alpha1.ParticipantRoleParticipant, IdentityProviderIssuer: "https://test-idp.example"},
			},
		},
	}
	leftAt := metav1.NewTime(time.Now().UTC().Add(-5 * time.Minute))
	leftParticipantSession := &breakglassv1alpha1.DebugSession{
		ObjectMeta: metav1.ObjectMeta{Name: "left-participant", Namespace: "default"},
		Spec:       breakglassv1alpha1.DebugSessionSpec{Cluster: "test-cluster", RequestedBy: "owner@example.com"},
		Status: breakglassv1alpha1.DebugSessionStatus{
			State: breakglassv1alpha1.DebugSessionStateActive,
			Participants: []breakglassv1alpha1.DebugSessionParticipant{
				{User: "user@example.com", Role: breakglassv1alpha1.ParticipantRoleParticipant, IdentityProviderIssuer: "https://test-idp.example", LeftAt: &leftAt},
			},
		},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(activeSession, otherSession, expiredSession).Build()
	handler := NewKubectlDebugHandler(client, &mockClientProvider{}).withIdentity(debugSessionReadIdentity{legacyAllowed: true})

	// Test finding the session (specific cluster)
	found, err := handler.FindActiveSessionForIssuer(context.Background(), "user@example.com", "test-cluster", "https://test-idp.example")
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, "active-session", found.Name)

	// Test wrong cluster
	found, err = handler.FindActiveSessionForIssuer(context.Background(), "user@example.com", "wrong-cluster", "https://test-idp.example")
	require.NoError(t, err)
	assert.Nil(t, found)

	// Test wildcard cluster
	found, err = handler.FindActiveSessionForIssuer(context.Background(), "user@example.com", "", "https://test-idp.example")
	require.NoError(t, err)
	require.NotNil(t, found)
	// Theoretically matches active-session or expired-session? No, expired should be ignored.
	// But wildcard might match active-session.
	assert.Equal(t, "active-session", found.Name)

	// Test wrong user
	found, err = handler.FindActiveSessionForIssuer(context.Background(), "other@example.com", "test-cluster", "https://test-idp.example")
	require.NoError(t, err)
	assert.Nil(t, found)

	// Test expired session
	// The expired session has status Active but ExpiresAt in past
	// FindActiveSession should filter it out
	// Create a client with ONLY expired session to valid
	clientExpired := fake.NewClientBuilder().WithScheme(scheme).WithObjects(expiredSession).Build()
	handlerExpired := NewKubectlDebugHandler(clientExpired, &mockClientProvider{}).withIdentity(debugSessionReadIdentity{legacyAllowed: true})
	found, err = handlerExpired.FindActiveSessionForIssuer(context.Background(), "user@example.com", "test-cluster", "https://test-idp.example")
	require.NoError(t, err)
	assert.Nil(t, found)

	clientLeft := fake.NewClientBuilder().WithScheme(scheme).WithObjects(leftParticipantSession).Build()
	handlerLeft := NewKubectlDebugHandler(clientLeft, &mockClientProvider{}).withIdentity(debugSessionReadIdentity{legacyAllowed: true})
	found, err = handlerLeft.FindActiveSessionForIssuer(context.Background(), "user@example.com", "test-cluster", "https://test-idp.example")
	require.NoError(t, err)
	assert.Nil(t, found)
}

func TestFindActiveSessionRejectsViewer(t *testing.T) {
	scheme := newKubectlTestScheme()
	session := &breakglassv1alpha1.DebugSession{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer-session", Namespace: "default"},
		Spec:       breakglassv1alpha1.DebugSessionSpec{Cluster: "test-cluster"},
		Status: breakglassv1alpha1.DebugSessionStatus{
			State:        breakglassv1alpha1.DebugSessionStateActive,
			Participants: []breakglassv1alpha1.DebugSessionParticipant{{User: "viewer@example.com", Role: breakglassv1alpha1.ParticipantRoleViewer, IdentityProviderIssuer: "https://trusted.example"}},
		},
	}
	handler := NewKubectlDebugHandler(fake.NewClientBuilder().WithScheme(scheme).WithObjects(session).Build(), nil)
	found, err := handler.FindActiveSessionForIssuer(context.Background(), "viewer@example.com", "test-cluster", "https://trusted.example")
	require.NoError(t, err)
	assert.Nil(t, found)
}

func TestEphemeralNamespaceLabelsUseSelectedSpoke(t *testing.T) {
	scheme := newKubectlTestScheme()
	hub := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target", Labels: map[string]string{"access": "yes"}}}).Build()
	spoke := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target", Labels: map[string]string{"access": "no"}}}).Build()
	handler := NewKubectlDebugHandler(hub, &mockClientProvider{clients: map[string]ctrlclient.Client{"spoke": spoke, "other": hub}})
	filter := &breakglassv1alpha1.NamespaceFilter{SelectorTerms: []breakglassv1alpha1.NamespaceSelectorTerm{{MatchLabels: map[string]string{"access": "yes"}}}}
	for _, tc := range []struct {
		cluster string
		want    bool
	}{{"spoke", false}, {"other", true}} {
		ds := &breakglassv1alpha1.DebugSession{Spec: breakglassv1alpha1.DebugSessionSpec{Cluster: tc.cluster}}
		got, err := handler.isNamespaceAllowedForEphemeral(context.Background(), ds, "target", filter, nil)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
}

func TestAdmissionClusterAdapterMissingProviderFailsClosed(t *testing.T) {
	client, err := AdaptClusterClientProvider(nil).GetClient(context.Background(), "spoke")
	require.Error(t, err)
	assert.Nil(t, client)
}

func TestFindActiveSessionPreservesIssuerAndRoleTogether(t *testing.T) {
	for _, tc := range []struct {
		name, issuer string
		role         breakglassv1alpha1.ParticipantRole
		want         bool
	}{
		{name: "same issuer participant", issuer: "https://trusted.example", role: breakglassv1alpha1.ParticipantRoleParticipant, want: true},
		{name: "same issuer owner", issuer: "https://trusted.example/", role: breakglassv1alpha1.ParticipantRoleOwner, want: true},
		{name: "wrong issuer participant", issuer: "https://other.example", role: breakglassv1alpha1.ParticipantRoleParticipant},
		{name: "missing issuer participant", role: breakglassv1alpha1.ParticipantRoleParticipant},
		{name: "same issuer viewer", issuer: "https://trusted.example", role: breakglassv1alpha1.ParticipantRoleViewer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &breakglassv1alpha1.DebugSession{ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default"}, Spec: breakglassv1alpha1.DebugSessionSpec{Cluster: "spoke"}, Status: breakglassv1alpha1.DebugSessionStatus{State: breakglassv1alpha1.DebugSessionStateActive, Participants: []breakglassv1alpha1.DebugSessionParticipant{{User: "shared@example.com", Role: tc.role, IdentityProviderIssuer: "https://trusted.example"}}}}
			handler := NewKubectlDebugHandler(fake.NewClientBuilder().WithScheme(newKubectlTestScheme()).WithObjects(session).Build(), nil)
			found, err := handler.FindActiveSessionForIssuer(context.Background(), "shared@example.com", "spoke", tc.issuer)
			require.NoError(t, err)
			assert.Equal(t, tc.want, found != nil)
		})
	}
}
