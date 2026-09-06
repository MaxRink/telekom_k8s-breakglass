// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
//
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"testing"

	breakglassv1alpha1 "github.com/telekom/k8s-breakglass/api/v1alpha1"
)

func TestNamespaceSelectorRefactorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expr   breakglassv1alpha1.NamespaceSelectorRequirement
		labels map[string]string
		want   bool
	}{
		{"in missing", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: breakglassv1alpha1.NamespaceSelectorOpIn, Values: []string{""}}, nil, false},
		{"in empty value", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: breakglassv1alpha1.NamespaceSelectorOpIn, Values: []string{""}}, map[string]string{"env": ""}, true},
		{"in nil values", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: breakglassv1alpha1.NamespaceSelectorOpIn}, map[string]string{"env": "prod"}, false},
		{"star is literal", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: breakglassv1alpha1.NamespaceSelectorOpIn, Values: []string{"*"}}, map[string]string{"env": "prod"}, false},
		{"literal star matches", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: breakglassv1alpha1.NamespaceSelectorOpIn, Values: []string{"*"}}, map[string]string{"env": "*"}, true},
		{"not in missing", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: breakglassv1alpha1.NamespaceSelectorOpNotIn, Values: []string{""}}, nil, true},
		{"not in empty value", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: breakglassv1alpha1.NamespaceSelectorOpNotIn, Values: []string{""}}, map[string]string{"env": ""}, false},
		{"not in nil values", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: breakglassv1alpha1.NamespaceSelectorOpNotIn}, map[string]string{"env": "prod"}, true},
		{"exists missing", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: breakglassv1alpha1.NamespaceSelectorOpExists}, nil, false},
		{"exists empty value", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: breakglassv1alpha1.NamespaceSelectorOpExists}, map[string]string{"env": ""}, true},
		{"does not exist", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: breakglassv1alpha1.NamespaceSelectorOpDoesNotExist}, nil, true},
		{"unknown fails closed", breakglassv1alpha1.NamespaceSelectorRequirement{Key: "env", Operator: "Unknown"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter := &breakglassv1alpha1.NamespaceFilter{
				SelectorTerms: []breakglassv1alpha1.NamespaceSelectorTerm{{
					MatchExpressions: []breakglassv1alpha1.NamespaceSelectorRequirement{tc.expr},
				}},
			}
			matcher := NewNamespaceMatcher(filter)
			if got := matcher.MatchesWithLabels("app", tc.labels); got != tc.want {
				t.Fatalf("MatchesWithLabels = %t, want %t", got, tc.want)
			}
			if matcher.Matches("app") {
				t.Fatal("name-only matching must not evaluate label selectors")
			}
			if tc.labels == nil && matcher.MatchesWithLabels("app", map[string]string{}) != tc.want {
				t.Fatal("nil and empty label maps must have the same semantics")
			}
		})
	}
}

func TestNamespaceDenyPrecedenceRefactor(t *testing.T) {
	matcher := NewNamespaceAllowDenyMatcher(
		&breakglassv1alpha1.NamespaceFilter{Patterns: []string{"*"}},
		&breakglassv1alpha1.NamespaceFilter{Patterns: []string{"kube-*"}},
	)
	if matcher.IsAllowed("kube-system") || matcher.IsAllowedWithLabels("kube-system", nil) {
		t.Fatal("deny must continue to take precedence over allow")
	}
	if !matcher.IsAllowed("app") || !matcher.IsAllowedWithLabels("app", nil) {
		t.Fatal("non-denied matching namespaces must remain allowed")
	}
	if !NewNamespaceAllowDenyMatcher(nil, nil).IsAllowed("app") {
		t.Fatal("empty allow and deny filters must continue to allow all namespaces")
	}
}
