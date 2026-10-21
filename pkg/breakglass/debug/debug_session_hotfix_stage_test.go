// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
//
// SPDX-License-Identifier: Apache-2.0

package debug

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	breakglassv1alpha1 "github.com/telekom/k8s-breakglass/api/v1alpha1"
	"github.com/telekom/k8s-breakglass/pkg/audit"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// An apply is requested through the API under the binding its plan was
// reviewed under.
func TestHotfixAPIApplyUnderTheBindingOfThePlan(t *testing.T) {
	f := newHotfixFixture(t, 1, 0)
	cc := &breakglassv1alpha1.ClusterConfig{}
	require.NoError(t, f.hub.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "spoke"}, cc))
	cc.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Test", LastTransitionTime: metav1.Now()}}
	cc.Status.ObservedGeneration = cc.Generation
	require.NoError(t, f.hub.Update(t.Context(), cc))
	binding := &breakglassv1alpha1.DebugSessionClusterBinding{ObjectMeta: metav1.ObjectMeta{Name: "platform", Namespace: "default"},
		Spec: breakglassv1alpha1.DebugSessionClusterBindingSpec{TemplateRef: &breakglassv1alpha1.TemplateReference{Name: "hotfix"},
			Clusters: []string{"spoke"}, Allowed: &breakglassv1alpha1.DebugSessionAllowed{Users: []string{"requester"}}}}
	require.NoError(t, f.hub.Create(t.Context(), binding))

	plan := f.planned(t, "plan-1", filePlanRequest(map[string]string{"pool": "w"}))
	// The API stored the plan with the cluster and binding of its request.
	plan.Spec.Cluster = "spoke"
	plan.Spec.BindingRef = &breakglassv1alpha1.BindingReference{Name: binding.Name, Namespace: binding.Namespace}
	require.NoError(t, f.hub.Update(t.Context(), plan))

	api := NewDebugSessionAPIController(zap.NewNop().Sugar(), f.hub, nil, nil)
	router := gin.New()
	router.Use(func(ctx *gin.Context) {
		ctx.Set("legacy_identity_allowed", true)
		ctx.Set("username", "requester")
		ctx.Set("email", "requester")
		ctx.Next()
	})
	require.NoError(t, api.Register(router.Group("/debugSessions")))
	body := `{"templateRef":"hotfix","cluster":"spoke","hotfix":{"stage":"apply","planRef":"plan-1","planHash":"` + plan.Status.Hotfix.PlanHash + `"}}`
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/debugSessions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	var created DebugSessionDetailResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &created))
	require.Equal(t, plan.Spec.BindingRef, created.Spec.BindingRef)
}

// A gate that fails during the soak restarts the soak; the next batch waits
// for a passing gate and the full soak again.
func TestHotfixGateFailureDuringSoakRestartsTheSoak(t *testing.T) {
	f := newHotfixFixture(t, 3, 0)
	plan := f.planned(t, "plan-1", filePlanRequest(map[string]string{"pool": "w"}))
	ds := f.session(t, "apply-1", &breakglassv1alpha1.DebugSessionHotfixRequest{
		Stage: breakglassv1alpha1.HotfixStageApply, PlanRef: "plan-1", PlanHash: plan.Status.Hotfix.PlanHash})
	started := func() int {
		n := 0
		for _, node := range ds.Status.Hotfix.Nodes {
			if node.CreateAttempted {
				n++
			}
		}
		return n
	}
	for i := 0; i < 50; i++ {
		_, err := f.c.handleHotfixActive(t.Context(), ds)
		require.NoError(t, err)
		f.executor(t)
		require.NoError(t, f.hub.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
		if ds.Status.Hotfix != nil && ds.Status.Hotfix.GatePassedAt != nil {
			break
		}
	}
	require.NotNil(t, ds.Status.Hotfix.GatePassedAt, "the canary batch soaks")
	require.Equal(t, 1, started())
	batch := ds.Status.Hotfix.CurrentBatch
	// The soak is over, but the gate fails now.
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	ds.Status.Hotfix.GatePassedAt = &past
	require.NoError(t, f.hub.Status().Update(t.Context(), ds))
	// The canary node reports memory pressure now.
	canary := &corev1.Node{}
	canaryName := ""
	for _, node := range ds.Status.Hotfix.Nodes {
		if node.CreateAttempted {
			canaryName = node.Name
		}
	}
	require.NoError(t, f.spoke.Get(t.Context(), client.ObjectKey{Name: canaryName}, canary))
	healthy := canary.Status.Conditions
	canary.Status.Conditions = append(append([]corev1.NodeCondition(nil), healthy...), corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue})
	require.NoError(t, f.spoke.Status().Update(t.Context(), canary))
	_, err := f.c.handleHotfixActive(t.Context(), ds)
	require.NoError(t, err)
	require.NoError(t, f.hub.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
	require.Nil(t, ds.Status.Hotfix.GatePassedAt, "a failed gate clears the soak")
	require.Equal(t, breakglassv1alpha1.HotfixPhaseRunning, ds.Status.Hotfix.Phase)
	require.Equal(t, batch, ds.Status.Hotfix.CurrentBatch)

	canary.Status.Conditions = healthy
	require.NoError(t, f.spoke.Status().Update(t.Context(), canary))
	// One pass: the template's 1 s soak must not elapse between passes.
	_, err = f.c.handleHotfixActive(t.Context(), ds)
	require.NoError(t, err)
	require.NoError(t, f.hub.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
	require.NotNil(t, ds.Status.Hotfix.GatePassedAt, "a passing gate starts a fresh soak")
	require.True(t, ds.Status.Hotfix.GatePassedAt.After(past.Time))
	require.Equal(t, batch, ds.Status.Hotfix.CurrentBatch, "the next batch waits for the full soak")
	require.Equal(t, 1, started())
}

// Session expiry during a rollout starts no further node; the running
// executor is still observed and recorded.
func TestHotfixExpiryDuringRolloutStartsNoFurtherNode(t *testing.T) {
	f := newHotfixFixture(t, 3, 0)
	ds := startApply(t, f, 3)
	past := metav1.NewTime(time.Now().Add(-time.Minute))
	ds.Status.ExpiresAt = &past
	require.NoError(t, f.hub.Status().Update(t.Context(), ds))
	// An expired Active session accepts only a terminal status write.
	_, err := f.c.handleHotfixActive(t.Context(), ds)
	require.NoError(t, err)
	require.NoError(t, f.hub.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
	require.Equal(t, breakglassv1alpha1.DebugSessionStateExpired, ds.Status.State, ds.Status.Message)
	require.Equal(t, breakglassv1alpha1.HotfixPhaseStopped, ds.Status.Hotfix.Phase)
	// Cleanup waits for the running executor, records it and releases.
	require.Error(t, f.c.cleanupHotfix(t.Context(), ds))
	for i := 0; i < 10; i++ {
		f.executor(t)
		require.NoError(t, f.hub.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
		if f.c.cleanupHotfix(t.Context(), ds) == nil {
			break
		}
	}
	require.NoError(t, f.hub.Get(t.Context(), client.ObjectKeyFromObject(ds), ds))
	require.True(t, ds.Status.Hotfix.Released)
	require.True(t, ds.Status.Hotfix.ResultAudited)
	require.Contains(t, eventTypes(f.sink), audit.EventDebugSessionHotfixResult)
	started := 0
	for _, node := range ds.Status.Hotfix.Nodes {
		if node.CreateAttempted {
			started++
		}
	}
	require.Equal(t, 1, started, "no node starts after the expiry")
	require.Equal(t, "applied", ds.Status.Hotfix.Nodes[0].Result, "the running executor is recorded")
	require.Empty(t, f.pods(t))
	record := &breakglassv1alpha1.HotfixRecord{}
	require.NoError(t, f.hub.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: ds.Status.Hotfix.RecordName}, record))
	require.Len(t, record.Status.Nodes, 1)
	require.Equal(t, breakglassv1alpha1.HotfixNodeApplied, record.Status.Nodes[0].State)
}

// A rollback that fails on a node keeps that node applied and the record open.
func TestHotfixFailedRollbackKeepsTheRecordOpen(t *testing.T) {
	f := newHotfixFixture(t, 2, 0)
	plan := f.planned(t, "plan-1", filePlanRequest(map[string]string{"pool": "w"}))
	apply := f.drive(t, f.session(t, "apply-1", &breakglassv1alpha1.DebugSessionHotfixRequest{
		Stage: breakglassv1alpha1.HotfixStageApply, PlanRef: "plan-1", PlanHash: plan.Status.Hotfix.PlanHash}))
	require.Equal(t, breakglassv1alpha1.DebugSessionStateTerminated, apply.Status.State, apply.Status.Message)
	f.failNode = "worker-00"
	rollback := f.drive(t, f.session(t, "rollback-1", &breakglassv1alpha1.DebugSessionHotfixRequest{
		Stage: breakglassv1alpha1.HotfixStageRollback, RecordName: apply.Status.Hotfix.RecordName}))
	require.Equal(t, breakglassv1alpha1.DebugSessionStateFailed, rollback.Status.State)
	record := &breakglassv1alpha1.HotfixRecord{}
	require.NoError(t, f.hub.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: apply.Status.Hotfix.RecordName}, record))
	for _, node := range record.Status.Nodes {
		if node.Name == "worker-00" {
			require.Equal(t, breakglassv1alpha1.HotfixNodeApplied, node.State, "a failed rollback leaves the hotfix in place")
			require.Contains(t, node.Message, "rollback")
		}
	}
	require.False(t, meta.IsStatusConditionTrue(record.Status.Conditions, string(breakglassv1alpha1.HotfixRecordConditionClosed)))
	require.True(t, meta.IsStatusConditionTrue(record.Status.Conditions, string(breakglassv1alpha1.HotfixRecordConditionActive)))
}
