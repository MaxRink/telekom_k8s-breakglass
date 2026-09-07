// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
// SPDX-License-Identifier: Apache-2.0

package debug

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	breakglassv1alpha1 "github.com/telekom/k8s-breakglass/api/v1alpha1"
	"github.com/telekom/k8s-breakglass/pkg/cluster"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestDeleteTrackedResourceIdentityAndLegacyRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, live, recovery string
		wantError, deleted             bool
	}{
		{"original", "original", "original", "", false, true},
		{"replacement", "original", "replacement", "", false, false},
		{"legacy missing", "", "", "", false, true},
		{"legacy existing requires operator", "", "original", "", true, false},
		{"legacy approved", "", "original", `{"v1/Pod/ns/pod":"original"}`, false, true},
		{"legacy replaced after inspection", "", "replacement", `{"v1/Pod/ns/pod":"original"}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: types.UID(tc.live)}}
			builder := fake.NewClientBuilder().WithScheme(testScheme())
			if tc.live != "" {
				builder.WithObjects(pod)
			}
			target := builder.Build()
			ref := pod.DeepCopy()
			ref.UID = types.UID(tc.recorded)
			session := &breakglassv1alpha1.DebugSession{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{LegacyCleanupUIDsAnnotation: tc.recovery}}}
			err := deleteTrackedResource(ctx, target, session, ref)
			if tc.wantError {
				require.ErrorContains(t, err, "operator")
			} else {
				require.NoError(t, err)
			}
			err = target.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})
			require.Equal(t, tc.deleted, apierrors.IsNotFound(err))
		})
	}
}

func TestDeleteTrackedResourceUsesUIDPrecondition(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: "original"}}
	target := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(pod).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			options := &client.DeleteOptions{}
			for _, opt := range opts {
				opt.ApplyToDelete(options)
			}
			require.NotNil(t, options.Preconditions)
			require.Equal(t, types.UID("original"), *options.Preconditions.UID)
			// Simulate replacement between the read and deletion: the API's UID
			// precondition must fail, not delete the new instance.
			return apierrors.NewConflict(corev1.Resource("pods"), obj.GetName(), nil)
		},
	}).Build()
	require.Error(t, deleteTrackedResource(context.Background(), target, nil, pod))
	require.NoError(t, target.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))
}

func TestLifecycleCleanupPathsPreserveReplacement(t *testing.T) {
	for _, kind := range []string{"deployed", "pod-template", "auxiliary", "copied"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: "replacement"}}
			target := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(pod).Build()
			ds := newTestDebugSession("session", "template", "cluster", "user@example.com")
			ds.Status.DeployedResources = []breakglassv1alpha1.DeployedResourceRef{{APIVersion: "v1", Kind: "Pod", Namespace: "ns", Name: "pod", UID: "original", Source: "debug-pod"}}
			ctrl := &DebugSessionController{log: zap.NewNop().Sugar()}
			switch kind {
			case "deployed":
				require.NoError(t, ctrl.cleanupDeployedResources(ctx, ds, target, false, false))
				require.Empty(t, ds.Status.DeployedResources)
			case "pod-template":
				ds.Status.PodTemplateResourceStatuses = []breakglassv1alpha1.PodTemplateResourceStatus{{APIVersion: "v1", Kind: "Pod", Namespace: "ns", ResourceName: "pod", UID: "original", Created: true}}
				require.NoError(t, ctrl.cleanupPodTemplateResources(ctx, ds, target))
			case "auxiliary":
				m := NewAuxiliaryResourceManager(zap.NewNop().Sugar(), target)
				require.NoError(t, m.deleteResource(ctx, target, breakglassv1alpha1.AuxiliaryResourceStatus{APIVersion: "v1", Kind: "Pod", Namespace: "ns", ResourceName: "pod", UID: "original"}, ds))
			case "copied":
				ds.Status.KubectlDebugStatus = &breakglassv1alpha1.KubectlDebugStatus{CopiedPods: []breakglassv1alpha1.CopiedPodRef{{CopyName: "pod", CopyNamespace: "ns", CopyUID: "original"}}}
				hub := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(ds).WithStatusSubresource(ds).Build()
				h := NewKubectlDebugHandler(hub, &mockClientProvider{clients: map[string]client.Client{"cluster": target}})
				require.NoError(t, h.CleanupKubectlDebugResources(ctx, ds))
			}
			live := &corev1.Pod{}
			require.NoError(t, target.Get(ctx, client.ObjectKeyFromObject(pod), live))
			require.Equal(t, types.UID("replacement"), live.UID)
		})
	}
}

func TestTrackedWorkloadPodMembership(t *testing.T) {
	controller := true
	template := corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "debug", Image: "debug:v1"}}}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "deployment", Namespace: "ns", UID: "deployment-uid"}, Spec: appsv1.DeploymentSpec{Template: template}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "ns", UID: "rs-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: &controller}}}, Spec: appsv1.ReplicaSetSpec{Template: template}}
	daemon := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "daemon", Namespace: "ns", UID: "daemon-uid"}, Spec: appsv1.DaemonSetSpec{Template: template}}
	target := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(deployment, rs, daemon).Build()
	for _, tc := range []struct {
		name, kind, refName, refUID, ownerKind, ownerName, ownerUID string
		controller, modified, want                                  bool
	}{
		{"direct", "Pod", "pod", "pod-uid", "", "", "", false, false, true},
		{"daemon", "DaemonSet", "daemon", "daemon-uid", "DaemonSet", "daemon", "daemon-uid", true, false, true},
		{"deployment", "Deployment", "deployment", "deployment-uid", "ReplicaSet", "rs", "rs-uid", true, false, true},
		{"label only", "DaemonSet", "daemon", "daemon-uid", "", "", "", false, false, false},
		{"noncontroller forgery", "DaemonSet", "daemon", "daemon-uid", "DaemonSet", "daemon", "daemon-uid", false, false, false},
		{"controller forgery unrelated spec", "DaemonSet", "daemon", "daemon-uid", "DaemonSet", "daemon", "daemon-uid", true, true, false},
		{"replaced controller", "DaemonSet", "daemon", "old-uid", "DaemonSet", "daemon", "old-uid", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", UID: "pod-uid"}, Spec: *template.Spec.DeepCopy()}
			if tc.ownerKind != "" {
				pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: tc.ownerKind, Name: tc.ownerName, UID: types.UID(tc.ownerUID), Controller: ptr.To(tc.controller)}}
			}
			if tc.modified {
				pod.Spec.Containers[0].Image = "database:production"
			}
			ds := &breakglassv1alpha1.DebugSession{Status: breakglassv1alpha1.DebugSessionStatus{DeployedResources: []breakglassv1alpha1.DeployedResourceRef{{APIVersion: "apps/v1", Kind: tc.kind, Name: tc.refName, Namespace: "ns", UID: tc.refUID, Source: "debug-pod"}}}}
			require.Equal(t, tc.want, (&DebugSessionController{}).podBelongsToTrackedWorkload(context.Background(), target, ds, pod))
		})
	}
}

func TestPodTemplateIdentityComesFromApplyResponse(t *testing.T) {
	target := fake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, cfg runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			return json.Unmarshal([]byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"config","namespace":"ns","uid":"applied-uid"}}`), cfg)
		},
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			t.Fatal("a second lookup could bind replacement UID")
			return nil
		},
	}).Build()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	obj.SetName("config")
	ds := &breakglassv1alpha1.DebugSession{}
	require.NoError(t, (&DebugSessionController{log: zap.NewNop().Sugar()}).deployPodTemplateResource(context.Background(), target, ds, obj, "ns"))
	require.Equal(t, "applied-uid", ds.Status.PodTemplateResourceStatuses[0].UID)
	require.Equal(t, "applied-uid", ds.Status.DeployedResources[0].UID)
}

func TestAuxiliaryReadinessUsesOneUIDCheckedSnapshot(t *testing.T) {
	gets := 0
	target := fake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(interceptor.Funcs{Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
		gets++
		obj.SetUID("original")
		return nil
	}}).Build()
	m := NewAuxiliaryResourceManager(zap.NewNop().Sugar(), target)
	result := m.checkSingleResourceReadiness(context.Background(), zap.NewNop().Sugar(), target, "v1", "ConfigMap", "config", "ns", "original")
	require.Equal(t, 1, gets)
	require.True(t, result.ready)
	gets = 0
	result = m.checkSingleResourceReadiness(context.Background(), zap.NewNop().Sugar(), target, "v1", "ConfigMap", "config", "ns", "other")
	require.Equal(t, 1, gets)
	require.True(t, result.failed)
}

func TestCleanupResourcesRetainsInventoryWhenClusterConfigMissing(t *testing.T) {
	ds := newTestDebugSession("session", "template", "missing-cluster", "user@example.com")
	ds.Status.DeployedResources = []breakglassv1alpha1.DeployedResourceRef{{APIVersion: "v1", Kind: "Pod", Name: "pod", Namespace: "ns", UID: "original", Source: "debug-pod"}}
	hub := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(ds).WithStatusSubresource(ds).Build()
	controller := NewDebugSessionController(zap.NewNop().Sugar(), hub, cluster.NewClientProvider(hub, zap.NewNop().Sugar()))
	require.Error(t, controller.cleanupResources(context.Background(), ds))
	require.Len(t, ds.Status.DeployedResources, 1)
	require.Equal(t, "original", ds.Status.DeployedResources[0].UID)
	saved := &breakglassv1alpha1.DebugSession{}
	require.NoError(t, hub.Get(context.Background(), client.ObjectKeyFromObject(ds), saved))
	require.Equal(t, ds.Status.DeployedResources, saved.Status.DeployedResources)
}

func TestTrackedApplyRetainsResponseIdentity(t *testing.T) {
	for _, obj := range []client.Object{
		&corev1.Pod{},
		&corev1.ResourceQuota{},
		&policyv1.PodDisruptionBudget{},
		&appsv1.Deployment{},
		&appsv1.DaemonSet{},
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
		}},
	} {
		t.Run(fmt.Sprintf("%T", obj), func(t *testing.T) {
			obj.SetName("tracked")
			obj.SetNamespace("ns")
			target := fake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(interceptor.Funcs{
				Apply: func(_ context.Context, _ client.WithWatch, cfg runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
					options := (&client.ApplyOptions{}).ApplyOptions(opts)
					require.Equal(t, "breakglass-controller", options.FieldManager)
					require.NotNil(t, options.Force)
					require.True(t, *options.Force)
					return json.Unmarshal([]byte(`{"metadata":{"name":"tracked","namespace":"ns","uid":"applied-original"}}`), cfg)
				},
				Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
					t.Fatal("must use apply response, not a replacement lookup")
					return nil
				},
			}).Build()
			require.NoError(t, applyTrackedResource(context.Background(), target, obj))
			require.Equal(t, types.UID("applied-original"), obj.GetUID())
		})
	}
}
