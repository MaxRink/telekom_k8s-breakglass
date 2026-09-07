// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	breakglassv1alpha1 "github.com/telekom/k8s-breakglass/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLegacyIdentityRequiresUniqueConfiguredProvider(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, breakglassv1alpha1.AddToScheme(scheme))
	a := &breakglassv1alpha1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: breakglassv1alpha1.IdentityProviderSpec{Issuer: "https://a.example/"}}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(a).Build()
	require.True(t, IsOnlyEnabledIdentityProvider(context.Background(), cli, "a", "https://a.example"))
	require.False(t, IsOnlyEnabledIdentityProvider(context.Background(), cli, "b", "https://a.example"))
	require.False(t, IsOnlyEnabledIdentityProvider(context.Background(), cli, "a", ""))
	// Authority-only providers use the same effective issuer as authentication.
	a.Spec.Issuer = ""
	a.Spec.OIDC.Authority = "https://a.example/"
	require.NoError(t, cli.Update(context.Background(), a))
	require.True(t, IsOnlyEnabledIdentityProvider(context.Background(), cli, "a", "https://a.example"))
	a.Spec.Issuer = "https://explicit.example"
	require.NoError(t, cli.Update(context.Background(), a))
	require.False(t, IsOnlyEnabledIdentityProvider(context.Background(), cli, "a", "https://a.example"))
	a.Spec.Issuer = ""
	require.NoError(t, cli.Update(context.Background(), a))
	// An invalid second provider still makes identity provenance ambiguous.
	b := &breakglassv1alpha1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "b"}}
	require.NoError(t, cli.Create(context.Background(), b))
	require.False(t, IsOnlyEnabledIdentityProvider(context.Background(), cli, "a", "https://a.example"))
	b.Spec.Disabled = true
	require.NoError(t, cli.Update(context.Background(), b))
	require.True(t, IsOnlyEnabledIdentityProvider(context.Background(), cli, "a", "https://a.example"))
}
