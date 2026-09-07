// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
// SPDX-License-Identifier: Apache-2.0

package debug

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	breakglassv1alpha1 "github.com/telekom/k8s-breakglass/api/v1alpha1"
	"github.com/telekom/k8s-breakglass/pkg/breakglass"
	"github.com/telekom/k8s-breakglass/pkg/quotas"
	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDebugQuotaScopesAcrossNamespacesAndBindings(t *testing.T) {
	one := int32(1)
	for _, scope := range []string{"template", "binding-total", "binding-user"} {
		t.Run(scope, func(t *testing.T) {
			template := &breakglassv1alpha1.DebugSessionTemplate{ObjectMeta: metav1.ObjectMeta{Name: "template", UID: "template-uid"}}
			binding := &breakglassv1alpha1.DebugSessionClusterBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: "bindings", UID: "binding-one"}}
			other := binding.DeepCopy()
			other.Name = "other"
			other.UID = "binding-two"
			switch scope {
			case "template":
				template.Spec.Constraints = &breakglassv1alpha1.DebugSessionConstraints{MaxConcurrentSessions: 1}
			case "binding-total":
				binding.Spec.MaxActiveSessionsTotal = &one
			case "binding-user":
				binding.Spec.MaxActiveSessionsPerUser = &one
			}
			candidate := func(name, ns, user string) *breakglassv1alpha1.DebugSession {
				return &breakglassv1alpha1.DebugSession{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name), Annotations: map[string]string{quotas.AdmissionAnnotation: quotas.Pending}}, Spec: breakglassv1alpha1.DebugSessionSpec{TemplateRef: template.Name, RequestedBy: user, RequestedByEmail: "shared@example.com", BindingRef: &breakglassv1alpha1.BindingReference{Name: binding.Name, Namespace: binding.Namespace}}}
			}
			a := candidate("first", "sessions-one", "username-one")
			b := candidate("second", "sessions-two", "username-two")
			if scope == "template" {
				b.Spec.BindingRef.Name = other.Name
			}
			cli := fake.NewClientBuilder().WithScheme(Scheme).WithStatusSubresource(&breakglassv1alpha1.DebugSession{}).WithObjects(template, binding, other, a, b).Build()
			c := NewDebugSessionController(zap.NewNop().Sugar(), cli, nil).WithAPIReader(cli).WithQuotaNamespace("controller")
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(a), a))
			require.NoError(t, c.admitDebugSession(t.Context(), a))
			restarted := NewDebugSessionController(zap.NewNop().Sugar(), cli, nil).WithAPIReader(cli).WithQuotaNamespace("controller")
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(b), b))
			require.ErrorIs(t, restarted.admitDebugSession(t.Context(), b), quotas.ErrFull)
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(b), b))
			assert.Equal(t, breakglassv1alpha1.DebugSessionStateFailed, b.Status.State)
			assert.Equal(t, "Session quota reached", b.Status.Message)
			require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(a), a))
			require.NoError(t, restarted.admitDebugSession(t.Context(), a))
		})
	}
}

func TestDebugQuotaBootstrapsLegacyResolvedBinding(t *testing.T) {
	one := int32(1)
	template := &breakglassv1alpha1.DebugSessionTemplate{ObjectMeta: metav1.ObjectMeta{Name: "template", UID: "template"}}
	binding := &breakglassv1alpha1.DebugSessionClusterBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: "bindings", UID: "binding"}, Spec: breakglassv1alpha1.DebugSessionClusterBindingSpec{MaxActiveSessionsTotal: &one}}
	legacy := &breakglassv1alpha1.DebugSession{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "old", UID: "legacy"}, Spec: breakglassv1alpha1.DebugSessionSpec{TemplateRef: template.Name}, Status: breakglassv1alpha1.DebugSessionStatus{State: breakglassv1alpha1.DebugSessionStateActive, ResolvedBinding: &breakglassv1alpha1.ResolvedBindingRef{Name: binding.Name, Namespace: binding.Namespace}}}
	candidate := &breakglassv1alpha1.DebugSession{ObjectMeta: metav1.ObjectMeta{Name: "new", Namespace: "new", UID: "new", Annotations: map[string]string{quotas.AdmissionAnnotation: quotas.Pending}}, Spec: breakglassv1alpha1.DebugSessionSpec{TemplateRef: template.Name, BindingRef: &breakglassv1alpha1.BindingReference{Name: binding.Name, Namespace: binding.Namespace}}}
	cli := fake.NewClientBuilder().WithScheme(Scheme).WithStatusSubresource(&breakglassv1alpha1.DebugSession{}).WithObjects(template, binding, legacy, candidate).Build()
	c := NewDebugSessionController(zap.NewNop().Sugar(), cli, nil).WithAPIReader(cli).WithQuotaNamespace("controller")
	require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(candidate), candidate))
	require.ErrorIs(t, c.admitDebugSession(t.Context(), candidate), quotas.ErrFull)
}

func TestDebugQuotaStatusCannotReviveTerminalSession(t *testing.T) {
	current := &breakglassv1alpha1.DebugSession{ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "ns", UID: "uid", Annotations: map[string]string{quotas.AdmissionAnnotation: quotas.Ready}}, Status: breakglassv1alpha1.DebugSessionStatus{State: breakglassv1alpha1.DebugSessionStatePending}}
	cli := fake.NewClientBuilder().WithScheme(Scheme).WithStatusSubresource(&breakglassv1alpha1.DebugSession{}).WithObjects(current).Build()
	require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(current), current))
	stale := current.DeepCopy()
	current.Status.State = breakglassv1alpha1.DebugSessionStateTerminated
	require.NoError(t, cli.Status().Update(t.Context(), current))
	stale.Status.State = breakglassv1alpha1.DebugSessionStateActive
	err := breakglass.ApplyDebugSessionStatus(t.Context(), cli, stale)
	require.True(t, apierrors.IsConflict(err), "late activation must be fenced: %v", err)
	require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(current), current))
	assert.Equal(t, breakglassv1alpha1.DebugSessionStateTerminated, current.Status.State)
}

func TestDebugQuotaReservedSessionSurvivesDeletedPolicy(t *testing.T) {
	old := &breakglassv1alpha1.DebugSessionTemplate{ObjectMeta: metav1.ObjectMeta{Name: "old", UID: "old-template"}}
	next := &breakglassv1alpha1.DebugSessionTemplate{ObjectMeta: metav1.ObjectMeta{Name: "next", UID: "next-template"}}
	candidate := func(name, template string) *breakglassv1alpha1.DebugSession {
		return &breakglassv1alpha1.DebugSession{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(name), Annotations: map[string]string{quotas.AdmissionAnnotation: quotas.Pending}}, Spec: breakglassv1alpha1.DebugSessionSpec{TemplateRef: template}}
	}
	first := candidate("first", old.Name)
	second := candidate("second", next.Name)
	cli := fake.NewClientBuilder().WithScheme(Scheme).WithStatusSubresource(&breakglassv1alpha1.DebugSession{}).WithObjects(old, next, first, second).Build()
	c := NewDebugSessionController(zap.NewNop().Sugar(), cli, nil).WithAPIReader(cli).WithQuotaNamespace("controller")
	require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(first), first))
	require.NoError(t, c.admitDebugSession(t.Context(), first))
	require.NoError(t, cli.Delete(t.Context(), old))
	require.NoError(t, cli.Get(t.Context(), client.ObjectKeyFromObject(second), second))
	require.NoError(t, c.admitDebugSession(t.Context(), second))
}

type failQuotaClusterConfigReader struct{ client.Reader }

func (r failQuotaClusterConfigReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*breakglassv1alpha1.ClusterConfigList); ok {
		return fmt.Errorf("cluster config unavailable")
	}
	return r.Reader.List(ctx, list, opts...)
}
func TestDebugQuotaLegacyBindingDiscoveryFailsClosed(t *testing.T) {
	template := &breakglassv1alpha1.DebugSessionTemplate{ObjectMeta: metav1.ObjectMeta{Name: "template", UID: "template"}}
	cli := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(template).Build()
	c := NewDebugSessionController(zap.NewNop().Sugar(), cli, nil).WithAPIReader(failQuotaClusterConfigReader{Reader: cli}).WithQuotaNamespace("controller")
	session := &breakglassv1alpha1.DebugSession{ObjectMeta: metav1.ObjectMeta{UID: "session"}, Spec: breakglassv1alpha1.DebugSessionSpec{TemplateRef: template.Name}}
	_, _, err := c.debugQuotaPolicy(t.Context(), session)
	require.ErrorContains(t, err, "cluster config unavailable")
}

func TestDebugQuotaLegacySelectorRequiresClusterConfig(t *testing.T) {
	template := &breakglassv1alpha1.DebugSessionTemplate{ObjectMeta: metav1.ObjectMeta{Name: "template", UID: "template"}}
	binding := &breakglassv1alpha1.DebugSessionClusterBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: "ns", UID: "binding"}, Spec: breakglassv1alpha1.DebugSessionClusterBindingSpec{TemplateRef: &breakglassv1alpha1.TemplateReference{Name: template.Name}, ClusterSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"environment": "prod"}}}}
	cli := fake.NewClientBuilder().WithScheme(Scheme).WithObjects(template, binding).Build()
	c := NewDebugSessionController(zap.NewNop().Sugar(), cli, nil).WithAPIReader(cli).WithQuotaNamespace("controller")
	session := &breakglassv1alpha1.DebugSession{ObjectMeta: metav1.ObjectMeta{UID: "session"}, Spec: breakglassv1alpha1.DebugSessionSpec{TemplateRef: template.Name, Cluster: "missing"}}
	_, _, err := c.debugQuotaPolicy(t.Context(), session)
	require.ErrorContains(t, err, "cluster config required")
}
