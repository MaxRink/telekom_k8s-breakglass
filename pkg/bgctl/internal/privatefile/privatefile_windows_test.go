// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package privatefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCreateTempPrivateUsesProtectedOwnerACL(t *testing.T) {
	f, err := createTempPrivate(t.TempDir(), ".private-")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer os.Remove(path)
	defer f.Close()
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	assertProtectedOwnerACL(t, sd)
}

func TestPrivateWriteAndReplacement(t *testing.T) {
	t.Setenv("USERNAME", "Everyone")
	path := filepath.Join(t.TempDir(), "tokens")
	for _, content := range []string{"first secret", "replacement"} {
		if err := Write(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("readback: %q %v", got, err)
		}
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		assertProtectedOwnerACL(t, sd)
	}
}

// Both initial creation and replacement must exclude inherited/group access.
// Resolve the actual token SID; the USERNAME environment variable is untrusted.
func assertProtectedOwnerACL(t *testing.T, sd *windows.SECURITY_DESCRIPTOR) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := sd.String()
	if !strings.Contains(descriptor, "D:P") || strings.Count(descriptor, "(") != 1 || !strings.Contains(descriptor, user.User.Sid.String()) {
		t.Fatalf("unexpected ACL: %s", descriptor)
	}
}
