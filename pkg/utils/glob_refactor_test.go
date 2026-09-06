// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
//
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"errors"
	"path"
	"testing"
)

func TestGlobMatchRefactorBoundaries(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		value   string
		want    bool
		bad     bool
	}{
		{"*", "", true, false},
		{"*", "team/admin", true, false}, // The universal wildcard is deliberately special.
		{"team/*", "team/admin", true, false},
		{"team/*", "team/admin/nested", false, false},
		{"ops-?", "ops-a", true, false},
		{"[ab]", "a", true, false},
		{"[ab]", "c", false, false},
		{"[", "a", false, true},
		{"a*[", "different", false, true},
		{"admin", "admin", true, false},
		{"admin", "Admin", false, false},
		{"", "", true, false},
		{`a\b`, `a\b`, true, false}, // Preserve the existing non-wildcard literal path.
	} {
		t.Run(tc.pattern+":"+tc.value, func(t *testing.T) {
			got, err := GlobMatch(tc.pattern, tc.value)
			if got != tc.want || errors.Is(err, path.ErrBadPattern) != tc.bad {
				t.Fatalf("GlobMatch(%q, %q) = (%t, %v); want (%t, badPattern=%t)", tc.pattern, tc.value, got, err, tc.want, tc.bad)
			}
		})
	}
}

func TestGlobMatchRefactorCollections(t *testing.T) {
	if GlobMatchAny(nil, "ops") || GlobMatchGroups(nil, []string{"ops"}) || GlobMatchGroups([]string{"*"}, nil) {
		t.Fatal("empty collections must not match")
	}
	if !GlobMatchAny([]string{"[", "ops-*"}, "ops-admin") {
		t.Fatal("invalid patterns must not prevent a later match")
	}
	if GlobMatchAny([]string{"[", "dev-*"}, "ops-admin") {
		t.Fatal("unmatched collections must remain false")
	}
	if !GlobMatchGroups([]string{"[", "ops-*"}, []string{"dev", "ops-admin"}) {
		t.Fatal("group matching must skip invalid patterns and find a later match")
	}
	if GlobMatchGroups([]string{"dev-*"}, []string{"ops-admin"}) {
		t.Fatal("unmatched groups must remain false")
	}
}
