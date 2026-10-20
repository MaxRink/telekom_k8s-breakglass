// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	breakglassv1alpha1 "github.com/telekom/k8s-breakglass/api/v1alpha1"
	"github.com/telekom/k8s-breakglass/pkg/metrics"
)

func currentManager(svc *Service) *Manager {
	svc.mu.RLock()
	defer svc.mu.RUnlock()
	return svc.manager
}

// The AuditConfig reconciler resyncs every 10 minutes with unchanged specs. A
// rebuild on each resync stopped the running pipeline and dropped the events
// it was retrying (reason "shutdown") during a broker outage.
func TestReloadWithUnchangedConfigKeepsThePipeline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(server.Close)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "webhook-auth", Namespace: "test-namespace"},
		Data:       map[string][]byte{"token": []byte("one")},
	}
	cli := fake.NewClientBuilder().WithScheme(newServiceTestScheme(t)).WithObjects(secret).Build()
	svc := NewService(cli, nil, zap.NewNop(), "test-namespace")
	t.Cleanup(func() { _ = svc.Close() })
	config := func(url string) []*breakglassv1alpha1.AuditConfig {
		return []*breakglassv1alpha1.AuditConfig{logAuditConfig("logs"), {
			ObjectMeta: metav1.ObjectMeta{Name: "webhook"},
			Spec: breakglassv1alpha1.AuditConfigSpec{Enabled: true, Sinks: []breakglassv1alpha1.AuditSinkConfig{{
				Name: "webhook", Type: breakglassv1alpha1.AuditSinkTypeWebhook,
				Webhook: &breakglassv1alpha1.WebhookSinkSpec{URL: url,
					AuthSecretRef: &breakglassv1alpha1.SecretKeySelector{Name: "webhook-auth", Namespace: "test-namespace"}},
			}}},
		}}
	}

	require.NoError(t, svc.ReloadMultiple(context.Background(), config(server.URL)))
	running := currentManager(svc)
	unchanged := testutil.ToFloat64(metrics.AuditConfigReloads.WithLabelValues("unchanged"))
	require.NoError(t, svc.ReloadMultiple(context.Background(), config(server.URL)))
	assert.Same(t, running, currentManager(svc), "an unchanged resync must not replace the pipeline")
	assert.InDelta(t, unchanged+1, testutil.ToFloat64(metrics.AuditConfigReloads.WithLabelValues("unchanged")), 0)

	// A rotated Secret is not watched; the resync must pick it up.
	secret.Data["token"] = []byte("two")
	require.NoError(t, cli.Update(context.Background(), secret))
	require.NoError(t, svc.ReloadMultiple(context.Background(), config(server.URL)))
	rotated := currentManager(svc)
	assert.NotSame(t, running, rotated, "a changed Secret must rebuild the pipeline")

	require.NoError(t, svc.ReloadMultiple(context.Background(), config(server.URL+"/changed")))
	assert.NotSame(t, rotated, currentManager(svc), "a changed spec must rebuild the pipeline")
	changed := currentManager(svc)
	require.NoError(t, svc.ReloadMultiple(context.Background(), config(server.URL + "/changed")[1:]))
	assert.NotSame(t, changed, currentManager(svc), "a removed AuditConfig must rebuild the pipeline")
}

// countingFlakySink fails its first writes, like a broker that comes back.
type countingFlakySink struct {
	failures  atomic.Int32
	delivered atomic.Int32
}

func (s *countingFlakySink) Write(context.Context, *Event) error {
	if s.failures.Add(-1) >= 0 {
		return errors.New("broker unavailable")
	}
	s.delivered.Add(1)
	return nil
}
func (s *countingFlakySink) Close() error { return nil }
func (s *countingFlakySink) Name() string { return "flaky" }

// On a real change the replaced pipeline must finish the events it is still
// retrying instead of dropping them when it is closed.
func TestReplacedPipelineDeliversRetryingEvents(t *testing.T) {
	sink := &countingFlakySink{}
	sink.failures.Store(3)
	queued := DefaultQueuedSinkConfig()
	queued.RetryInitialBackoff, queued.RetryMaxBackoff, queued.RetryTimeout = 100*time.Millisecond, 100*time.Millisecond, 10*time.Second
	multi := NewIsolatedMultiSink([]Sink{sink}, queued, zap.NewNop())
	svc := NewService(fake.NewClientBuilder().WithScheme(newServiceTestScheme(t)).Build(), nil, zap.NewNop(), "test-namespace")
	svc.reloadMu.Lock()
	svc.swap(NewManager(multi, DefaultManagerConfig(), zap.NewNop()), []Sink{sink}, multi, nil, true, ConfigurationReady)
	svc.reloadMu.Unlock()

	svc.Emit(context.Background(), &Event{Type: EventSessionDropped})
	require.Eventually(t, func() bool { return sink.failures.Load() < 3 }, 2*time.Second, 5*time.Millisecond, "delivery must be retrying")
	svc.reloadMu.Lock()
	svc.swap(nil, nil, nil, nil, false, ConfigurationDisabled)
	svc.reloadMu.Unlock()
	svc.closing.Wait()

	assert.EqualValues(t, 1, sink.delivered.Load(), "the retried event reaches the sink")
	assert.Zero(t, multi.Health()[0].DroppedEvents)
}

// The AuditConfig status reports drops of the per-sink queues too.
func TestStatsIncludeSinkQueueDrops(t *testing.T) {
	sink := &countingFlakySink{}
	sink.failures.Store(1 << 20)
	queued := DefaultQueuedSinkConfig()
	queued.RetryAttempts = 1
	multi := NewIsolatedMultiSink([]Sink{sink}, queued, zap.NewNop())
	svc := NewService(fake.NewClientBuilder().WithScheme(newServiceTestScheme(t)).Build(), nil, zap.NewNop(), "test-namespace")
	t.Cleanup(func() { _ = svc.Close() })
	svc.reloadMu.Lock()
	svc.swap(NewManager(multi, DefaultManagerConfig(), zap.NewNop()), []Sink{sink}, multi, nil, true, ConfigurationReady)
	svc.reloadMu.Unlock()

	svc.Emit(context.Background(), &Event{Type: EventSessionDropped})
	require.Eventually(t, func() bool { return svc.GetStats().DroppedEvents == 1 }, 2*time.Second, 10*time.Millisecond)
}
