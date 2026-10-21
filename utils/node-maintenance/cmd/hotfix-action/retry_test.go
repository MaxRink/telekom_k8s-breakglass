// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const oldCRAImage = "cit-t-caas-oci/images/t-caas/cra-vsr-6wind-ubi:3.12.3.1"

// hostContainerApplied applies a host-container hotfix and returns the
// rollback request.
func hostContainerApplied(t *testing.T, ex *executor, fake *fakeHost) (*request, string) {
	t.Helper()
	writeHost(t, ex, "/etc/cra/image", oldCRAImage+"\n")
	image := "registry.example/t-caas/cra-vsr" + digest
	c := &hostContainer{Name: "cra-vsr", ContainerdNamespace: "hbr", Unit: "cra.service", ImageRefFile: "/etc/cra/image"}
	req := &request{Stage: stagePlan, HotfixID: "hf-5", DeadlineUnix: time.Now().Add(time.Hour).Unix(), HostContainer: c,
		Payload: payload{Type: payloadHostContainer, Image: &imagePayload{HostContainer: "cra-vsr", Image: image}}}
	plan, err := ex.execute(req)
	require(t, err)
	req.Stage = stageApply
	req.Expected = &expected{BeforeSHA256: plan.BeforeSHA256, AfterSHA256: plan.AfterSHA256}
	running := oldCRAImage
	fake.answers["nerdctl --namespace hbr inspect"] = running + " true\n"
	fake.onCall = func(call string) {
		if call == "systemctl restart cra.service" {
			fake.answers["nerdctl --namespace hbr inspect"] = strings.TrimSpace(readHost(t, ex, "/etc/cra/image")) + " true\n"
		}
	}
	out, err := ex.execute(req)
	require(t, err)
	if out.Result != "applied" {
		t.Fatalf("apply %+v", out)
	}
	req.Stage = stageRollback
	return req, image
}

func TestHostContainerRollbackRetryRestartsAfterRestoredFile(t *testing.T) {
	ex, fake := newTestExecutor(t, true)
	req, image := hostContainerApplied(t, ex, fake)

	// An earlier rollback restored the file and failed before the restart.
	writeHost(t, ex, "/etc/cra/image", oldCRAImage+"\n")
	fake.answers["nerdctl --namespace hbr inspect"] = image + " true\n"
	fake.calls = nil
	out, err := ex.execute(req)
	require(t, err)
	if out.Result != "skipped" || !strings.Contains(strings.Join(fake.calls, ";"), "systemctl restart cra.service") {
		t.Fatalf("the retry must restart the container on the previous image: %+v %v", out, fake.calls)
	}
	if got := fake.answers["nerdctl --namespace hbr inspect"]; got != oldCRAImage+" true\n" {
		t.Fatalf("container runs %q after the retry", got)
	}

	// Once the previous image runs, a further retry changes nothing.
	fake.calls = nil
	out, err = ex.execute(req)
	require(t, err)
	if out.Result != "skipped" || strings.Contains(strings.Join(fake.calls, ";"), "systemctl restart") {
		t.Fatalf("a finished rollback must not restart again: %+v %v", out, fake.calls)
	}
}

func TestHostContainerRollbackRetryRestartsAStoppedContainer(t *testing.T) {
	ex, fake := newTestExecutor(t, true)
	req, _ := hostContainerApplied(t, ex, fake)
	// An earlier rollback restored the file; after its restart the container
	// exists but does not run.
	writeHost(t, ex, "/etc/cra/image", oldCRAImage+"\n")
	fake.answers["nerdctl --namespace hbr inspect"] = oldCRAImage + " false\n"
	fake.calls = nil
	out, err := ex.execute(req)
	require(t, err)
	if out.Result != "skipped" || !strings.Contains(strings.Join(fake.calls, ";"), "systemctl restart cra.service") ||
		fake.answers["nerdctl --namespace hbr inspect"] != oldCRAImage+" true\n" {
		t.Fatalf("a stopped container must be restarted on the previous image: %+v %v", out, fake.calls)
	}
}

func TestFileRollbackRetryRestartsAfterRestoredFile(t *testing.T) {
	ex, fake := newTestExecutor(t, true)
	writeHost(t, ex, "/etc/containerd/config.toml", "version = 2\n")
	content := "version = 3\n"
	f := filePayload{Path: "/etc/containerd/config.toml", Mode: "replace", Content: content,
		BeforeSHA256: []string{sum([]byte("version = 2\n"))}, AfterSHA256: sum([]byte(content))}
	req := fileRequest(stagePlan, f)
	req.Payload.Restart = []string{"containerd.service"}
	plan, err := ex.execute(req)
	require(t, err)
	req.Stage = stageApply
	req.Expected = &expected{BeforeSHA256: plan.BeforeSHA256, AfterSHA256: plan.AfterSHA256}
	fake.answers["systemctl is-active"] = "active\n"
	_, err = ex.execute(req)
	require(t, err)
	// An earlier rollback restored the file and failed before the restart.
	writeHost(t, ex, "/etc/containerd/config.toml", "version = 2\n")
	req.Stage = stageRollback
	fake.calls = nil
	out, err := ex.execute(req)
	require(t, err)
	if out.Result != "skipped" || !strings.Contains(strings.Join(fake.calls, ";"), "systemctl restart containerd.service") {
		t.Fatalf("the retry must restart the units: %+v %v", out, fake.calls)
	}
	// A node the hotfix never reached has no backup and is not restarted.
	other, otherFake := newTestExecutor(t, true)
	writeHost(t, other, "/etc/containerd/config.toml", "version = 2\n")
	if out, err := other.execute(req); err != nil || out.Result != "skipped" || len(otherFake.calls) != 0 {
		t.Fatalf("an untouched node must be skipped without a restart: %+v %v %v", out, err, otherFake.calls)
	}
}

func TestHostContainerRollbackKeepsFileWithoutPreviousImage(t *testing.T) {
	ex, fake := newTestExecutor(t, true)
	req, image := hostContainerApplied(t, ex, fake)
	require(t, os.Remove(ex.host(ex.runningImagePath(req, req.HostContainer))))
	if _, err := ex.execute(req); err == nil {
		t.Fatal("rollback must fail when the previous image is unknown")
	}
	if readHost(t, ex, "/etc/cra/image") != image+"\n" {
		t.Fatal("the file must stay at the hotfix image when the previous image is unknown")
	}
}

func TestHostContainerApplyRetryRestartsWhenOldImageRuns(t *testing.T) {
	ex, fake := newTestExecutor(t, true)
	req, image := hostContainerApplied(t, ex, fake)
	req.Stage = stageApply
	// The file is at the hotfix image, but the old image still runs.
	fake.answers["nerdctl --namespace hbr inspect"] = oldCRAImage + " true\n"
	fake.calls = nil
	out, err := ex.execute(req)
	require(t, err)
	if out.Result != "skipped" || !strings.Contains(strings.Join(fake.calls, ";"), "systemctl restart cra.service") ||
		fake.answers["nerdctl --namespace hbr inspect"] != image+" true\n" {
		t.Fatalf("an apply retry must restart onto the hotfix image: %+v %v", out, fake.calls)
	}
}

func TestStaticPodApplyRetryWaitsForTheImage(t *testing.T) {
	ex, fake := newTestExecutor(t, true)
	image := "registry.k8s.io/kube-apiserver" + digest
	updated, err := patchImage([]byte(apiserverManifest), image)
	require(t, err)
	writeHost(t, ex, manifestPath("kube-apiserver"), string(updated))
	req := &request{Stage: stageApply, HotfixID: "hf-3", DeadlineUnix: time.Now().Add(time.Hour).Unix(),
		Payload:  payload{Type: payloadStaticPod, Image: &imagePayload{StaticPod: "kube-apiserver", Image: image}},
		Expected: &expected{BeforeSHA256: sum([]byte(apiserverManifest)), AfterSHA256: sum(updated)}}
	ex.deadline = time.Now().Add(time.Hour)
	fake.answers["crictl inspecti"] = `{"status":{"id":"sha256:new","repoDigests":["registry.k8s.io/kube-apiserver` + digest + `"]}}`
	fake.answers["crictl ps --name ^kube-apiserver$"] = `{"containers":[{"id":"c0","imageRef":"sha256:old"}]}`
	start := time.Now()
	ex.now = func() time.Time { return start.Add(time.Since(start) * 1000) }
	if _, err := ex.staticPod(req); err == nil {
		t.Fatal("an apply retry must not succeed while the old image runs")
	}
	fake.answers["crictl ps --name ^kube-apiserver$"] = `{"containers":[{"id":"c1","imageRef":"sha256:new"}]}`
	ex.now = time.Now
	out, err := ex.execute(req)
	require(t, err)
	if out.Result != "skipped" {
		t.Fatalf("retry %+v", out)
	}
}

func TestEtcdRollbackRequiresQuorumOnlyForARunningMember(t *testing.T) {
	ex, fake := newTestExecutor(t, true)
	manifest := strings.ReplaceAll(apiserverManifest, "kube-apiserver", "etcd")
	image := "registry.k8s.io/etcd" + digest
	updated, err := patchImage([]byte(manifest), image)
	require(t, err)
	writeHost(t, ex, manifestPath("etcd"), string(updated))
	req := &request{Stage: stageRollback, HotfixID: "hf-4", DeadlineUnix: time.Now().Add(time.Hour).Unix(),
		Payload:  payload{Type: payloadStaticPod, Image: &imagePayload{StaticPod: "etcd", Image: image}},
		Expected: &expected{BeforeSHA256: sum([]byte(manifest)), AfterSHA256: sum(updated)}}
	backup := ex.host(ex.backupPath(req, manifestPath("etcd")))
	require(t, os.MkdirAll(filepath.Dir(backup), 0o700))
	require(t, os.WriteFile(backup, []byte(manifest), 0o600))

	// A running member without full quorum is not restarted.
	fake.answers["crictl ps --name ^etcd$ --label io.kubernetes.pod.namespace=kube-system --state running -q"] = "e1\n"
	fake.answers["crictl exec e1 etcdctl"] = `{"members":[{"ID":1},{"ID":2},{"ID":3}]}`
	fake.onCall = func(call string) {
		if strings.HasSuffix(call, "endpoint status --cluster") {
			fake.answers["crictl exec e1 etcdctl"] = `[{"Endpoint":"a","Status":{"header":{"member_id":1},"leader":7}},{"Endpoint":"b","Status":{"header":{"member_id":2},"leader":7}}]`
		}
	}
	if _, err := ex.execute(req); err == nil || !strings.Contains(err.Error(), "quorum") {
		t.Fatalf("2 of 3 members must refuse the rollback of a running member, got %v", err)
	}
	if readHost(t, ex, manifestPath("etcd")) != string(updated) {
		t.Fatal("manifest restored without quorum")
	}

	// A member whose container does not run can be rolled back; the
	// rollback waits until it runs again with the restored manifest.
	fake.answers["crictl ps --name ^etcd$ --label io.kubernetes.pod.namespace=kube-system --state running -q"] = ""
	fake.answers["crictl ps --name ^etcd$ --label io.kubernetes.pod.namespace=kube-system --state running -o json"] = `{"containers":[]}`
	fake.onCall = func(call string) {
		// After the restore the member comes back and the cluster is healthy.
		if strings.HasSuffix(call, "--state running -o json") && readHost(t, ex, manifestPath("etcd")) == manifest {
			fake.answers["crictl ps --name ^etcd$ --label io.kubernetes.pod.namespace=kube-system --state running -o json"] = `{"containers":[{"id":"e2","podSandboxId":"p2","imageRef":"sha256:old"}]}`
			fake.answers["crictl ps --name ^etcd$ --label io.kubernetes.pod.namespace=kube-system --state running -q"] = "e2\n"
		}
		if strings.HasSuffix(call, "member list") {
			fake.answers["crictl exec e2 etcdctl"] = `{"members":[{"ID":1},{"ID":2},{"ID":3}]}`
		}
		if strings.HasSuffix(call, "endpoint status --cluster") {
			fake.answers["crictl exec e2 etcdctl"] = `[{"Status":{"header":{"member_id":1},"leader":7}},{"Status":{"header":{"member_id":2},"leader":7}},{"Status":{"header":{"member_id":3},"leader":7}}]`
		}
	}
	out, err := ex.execute(req)
	require(t, err)
	if out.Result != "rolledBack" || readHost(t, ex, manifestPath("etcd")) != manifest {
		t.Fatalf("rollback %+v", out)
	}
}

func TestStaticPodRollbackWaitsForTheRestart(t *testing.T) {
	ex, fake := newTestExecutor(t, true)
	// The hotfix image has the same image ID as the original one, as in
	// the Kind proof: only a new container proves the restart.
	image := "registry.k8s.io/kube-apiserver" + digest
	updated, err := patchImage([]byte(apiserverManifest), image)
	require(t, err)
	writeHost(t, ex, manifestPath("kube-apiserver"), string(updated))
	req := &request{Stage: stageRollback, HotfixID: "hf-3", DeadlineUnix: time.Now().Add(time.Hour).Unix(),
		Payload:  payload{Type: payloadStaticPod, Image: &imagePayload{StaticPod: "kube-apiserver", Image: image}},
		Expected: &expected{BeforeSHA256: sum([]byte(apiserverManifest)), AfterSHA256: sum(updated)}}
	backup := ex.host(ex.backupPath(req, manifestPath("kube-apiserver")))
	require(t, os.MkdirAll(filepath.Dir(backup), 0o700))
	require(t, os.WriteFile(backup, []byte(apiserverManifest), 0o600))
	fake.answers["crictl ps --name ^kube-apiserver$"] = `{"containers":[{"id":"c1","podSandboxId":"s1","imageRef":"sha256:same"}]}`
	ex.deadline = time.Now().Add(time.Hour)
	start := time.Now()
	ex.now = func() time.Time { return start.Add(time.Since(start) * 1000) }
	if _, err := ex.staticPod(req); err == nil || !strings.Contains(err.Error(), "did not restart") {
		t.Fatalf("the rollback must wait for a new Pod sandbox, got %v", err)
	}
	// A crash restart in the old sandbox does not prove the restored manifest.
	writeHost(t, ex, manifestPath("kube-apiserver"), string(updated))
	fake.onCall = func(call string) {
		if strings.HasPrefix(call, "crictl ps --name ^kube-apiserver$") && readHost(t, ex, manifestPath("kube-apiserver")) == apiserverManifest {
			fake.answers["crictl ps --name ^kube-apiserver$"] = `{"containers":[{"id":"c9","podSandboxId":"s1","imageRef":"sha256:same"}]}`
		}
	}
	if _, err := ex.staticPod(req); err == nil || !strings.Contains(err.Error(), "did not restart") {
		t.Fatalf("a crash restart in the old sandbox must not pass, got %v", err)
	}
	fake.answers["crictl ps --name ^kube-apiserver$"] = `{"containers":[{"id":"c1","podSandboxId":"s1","imageRef":"sha256:same"}]}`
	if readHost(t, ex, manifestPath("kube-apiserver")) != apiserverManifest {
		t.Fatal("the manifest is restored before the wait")
	}

	// Run again from the hotfix manifest; kubelet restarts the Pod.
	writeHost(t, ex, manifestPath("kube-apiserver"), string(updated))
	fake.onCall = func(call string) {
		if strings.HasPrefix(call, "crictl ps --name ^kube-apiserver$") && readHost(t, ex, manifestPath("kube-apiserver")) == apiserverManifest {
			fake.answers["crictl ps --name ^kube-apiserver$"] = `{"containers":[{"id":"c2","podSandboxId":"s2","imageRef":"sha256:same"}]}`
		}
	}
	ex.now = time.Now
	out, err := ex.execute(req)
	require(t, err)
	if out.Result != "rolledBack" {
		t.Fatalf("rollback %+v", out)
	}
}

func TestStaticPodRollbackRetryWaitsForThePod(t *testing.T) {
	ex, fake := newTestExecutor(t, true)
	image := "registry.k8s.io/kube-apiserver" + digest
	updated, err := patchImage([]byte(apiserverManifest), image)
	require(t, err)
	// An earlier rollback restored the manifest and failed in the wait.
	writeHost(t, ex, manifestPath("kube-apiserver"), apiserverManifest)
	req := &request{Stage: stageRollback, HotfixID: "hf-3", DeadlineUnix: time.Now().Add(time.Hour).Unix(),
		Payload:  payload{Type: payloadStaticPod, Image: &imagePayload{StaticPod: "kube-apiserver", Image: image}},
		Expected: &expected{BeforeSHA256: sum([]byte(apiserverManifest)), AfterSHA256: sum(updated)}}
	backup := ex.host(ex.backupPath(req, manifestPath("kube-apiserver")))
	require(t, os.MkdirAll(filepath.Dir(backup), 0o700))
	require(t, os.WriteFile(backup, []byte(apiserverManifest), 0o600))
	require(t, os.WriteFile(backup+".sandboxes", []byte("s1"), 0o600))
	fake.answers["crictl ps --name ^kube-apiserver$"] = `{"containers":[]}`
	ex.deadline = time.Now().Add(time.Hour)
	start := time.Now()
	ex.now = func() time.Time { return start.Add(time.Since(start) * 1000) }
	if _, err := ex.staticPod(req); err == nil || !strings.Contains(err.Error(), "did not restart") {
		t.Fatalf("a retry must wait until the Pod runs, got %v", err)
	}
	// The hotfix Pod still runs in the sandbox recorded by the first attempt.
	fake.answers["crictl ps --name ^kube-apiserver$"] = `{"containers":[{"id":"c1","podSandboxId":"s1","imageRef":"sha256:same"}]}`
	if _, err := ex.staticPod(req); err == nil || !strings.Contains(err.Error(), "did not restart") {
		t.Fatalf("a retry must refuse the old sandbox, got %v", err)
	}
	fake.answers["crictl ps --name ^kube-apiserver$"] = `{"containers":[{"id":"c2","podSandboxId":"s2","imageRef":"sha256:old"}]}`
	out, err := ex.staticPod(req)
	require(t, err)
	if out.Result != "skipped" {
		t.Fatalf("retry %+v", out)
	}

	// A node the hotfix never reached is skipped without any wait.
	other, otherFake := newTestExecutor(t, true)
	writeHost(t, other, manifestPath("kube-apiserver"), apiserverManifest)
	other.deadline = time.Now().Add(time.Hour)
	if out, err := other.staticPod(req); err != nil || out.Result != "skipped" || len(otherFake.calls) != 0 {
		t.Fatalf("untouched node: %+v %v %v", out, err, otherFake.calls)
	}
}

func TestFetchToFileChecksIntegrity(t *testing.T) {
	body := []byte("deb payload\n")
	digestHex := sha256.Sum256(body)
	want := hex.EncodeToString(digestHex[:])
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write(body)
		case "/missing":
			http.NotFound(w, r)
		case "/plain":
			http.Redirect(w, r, "http://example.invalid/ok", http.StatusFound)
		}
	}))
	defer srv.Close()

	ex, _ := newTestExecutor(t, true)
	ex.deadline = time.Now().Add(time.Hour)
	dest := filepath.Join(ex.work, "out")

	// The node CA bundle is required; nothing is downloaded without it.
	if err := ex.fetchToFile(srv.URL+"/ok", want, dest, 1024); err == nil || !strings.Contains(err.Error(), "CA bundle") {
		t.Fatalf("missing CA bundle must fail, got %v", err)
	}
	writeHost(t, ex, "/etc/ssl/certs/ca-certificates.crt", "")
	if err := ex.fetchToFile(srv.URL+"/ok", want, dest, 1024); err == nil || !strings.Contains(err.Error(), "CA bundle") {
		t.Fatalf("an empty CA bundle must fail, got %v", err)
	}
	// With the server certificate in the node bundle, the production client
	// verifies TLS against that bundle (the test certificate is in no system
	// trust store).
	writeHost(t, ex, "/etc/ssl/certs/ca-certificates.crt", testCAPEM(t))
	require(t, ex.fetchToFile(srv.URL+"/ok", want, dest, 1024))
	if got, _ := os.ReadFile(dest); string(got) != string(body) {
		t.Fatalf("downloaded %q", got)
	}
	for name, tc := range map[string]struct {
		path, want string
		limit      int64
		err        string
	}{
		"sha mismatch":  {"/ok", strings.Repeat("0", 64), 1024, "sha256"},
		"size cap":      {"/ok", want, 4, "larger than"},
		"not found":     {"/missing", want, 1024, "HTTP 404"},
		"http redirect": {"/plain", want, 1024, "redirect only to HTTPS"},
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(ex.work, strings.ReplaceAll(name, " ", "-"))
			err := ex.fetchToFile(srv.URL+tc.path, tc.want, out, tc.limit)
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("want error containing %q, got %v", tc.err, err)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Fatal("a failed download must not leave the destination")
			}
			if _, statErr := os.Stat(out + ".download"); !os.IsNotExist(statErr) {
				t.Fatal("a failed download must not leave a partial file")
			}
		})
	}
}

func TestHTTPClientRefusesPlainRedirects(t *testing.T) {
	ex, _ := newTestExecutor(t, true)
	writeHost(t, ex, "/etc/ssl/certs/ca-certificates.crt", testCAPEM(t))
	client, err := ex.httpClient()
	require(t, err)
	next, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	if err := client.CheckRedirect(next, nil); err == nil {
		t.Fatal("a redirect to plain HTTP must be refused")
	}
	next, _ = http.NewRequest(http.MethodGet, "https://example.invalid/x", nil)
	if err := client.CheckRedirect(next, make([]*http.Request, 5)); err == nil {
		t.Fatal("more than 5 redirects must be refused")
	}
	if err := client.CheckRedirect(next, nil); err != nil {
		t.Fatalf("an HTTPS redirect is allowed: %v", err)
	}
}

// testCAPEM returns the PEM certificate of a throwaway TLS server.
func testCAPEM(t *testing.T) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}
