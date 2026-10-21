// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestCRDSchema_HotfixRecordImmutability proves that the API server keeps the
// recorded change fixed and admits only lifecycle updates.
func TestCRDSchema_HotfixRecordImmutability(t *testing.T) {
	c := impersonationEnvtestClient(t)
	ctx := context.Background()

	request := testHotfixPlanRequest()
	record := &HotfixRecord{
		ObjectMeta: metav1.ObjectMeta{Name: "hotfix-schema", Namespace: "default"},
		Spec: HotfixRecordSpec{
			Cluster:       "spoke",
			TemplateRef:   "platform-hotfix",
			Payload:       *request.Payload,
			PlanHash:      strings.Repeat("c", 64),
			Rollout:       HotfixRollout{Strategy: HotfixRolloutCanary, CanaryNodes: 1, SoakSeconds: 60},
			NodeSelection: *request.Nodes,
			FixedIn:       request.FixedIn,
			ExpiresAt:     metav1.NewTime(time.Now().Add(14 * 24 * time.Hour).Truncate(time.Second)),
			Sessions: []HotfixRecordSession{{
				Stage: HotfixStageApply, Name: "apply", UID: "uid-apply",
				RequestedBy: "requester", ApprovedBy: "approver", At: metav1.NewTime(time.Now().Truncate(time.Second)),
			}},
		},
	}
	if err := c.Create(ctx, record); err != nil {
		t.Fatalf("create: %v", err)
	}

	lifecycle := record.DeepCopy()
	lifecycle.Spec.ExpiresAt = metav1.NewTime(lifecycle.Spec.ExpiresAt.Add(24 * time.Hour))
	lifecycle.Spec.FixedIn = "node image 2026.11.1"
	lifecycle.Spec.Sessions = append(lifecycle.Spec.Sessions, HotfixRecordSession{
		Stage: HotfixStageReconfirm, Name: "reconfirm", UID: "uid-reconfirm",
		RequestedBy: "requester", ApprovedBy: "approver", At: metav1.NewTime(time.Now().Truncate(time.Second)),
	})
	if err := c.Update(ctx, lifecycle); err != nil {
		t.Fatalf("lifecycle update: %v", err)
	}

	for name, mutate := range map[string]func(*HotfixRecordSpec){
		"cluster":       func(s *HotfixRecordSpec) { s.Cluster = "other" },
		"templateRef":   func(s *HotfixRecordSpec) { s.TemplateRef = "other" },
		"payload":       func(s *HotfixRecordSpec) { s.Payload.File.BeforeSHA256 = []string{strings.Repeat("d", 64)} },
		"planHash":      func(s *HotfixRecordSpec) { s.PlanHash = strings.Repeat("e", 64) },
		"rollout":       func(s *HotfixRecordSpec) { s.Rollout.SoakSeconds = 120 },
		"nodeSelection": func(s *HotfixRecordSpec) { s.NodeSelection.Selector = nil; s.NodeSelection.Names = []string{"node-1"} },
	} {
		t.Run(name, func(t *testing.T) {
			changed := lifecycle.DeepCopy()
			mutate(&changed.Spec)
			err := c.Update(ctx, changed)
			if err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Fatalf("want immutable error, got %v", err)
			}
		})
	}
}
