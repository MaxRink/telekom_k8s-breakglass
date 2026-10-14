// SPDX-FileCopyrightText: 2026 Deutsche Telekom AG
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

const hostPath = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// runCommand runs one binary without a shell and returns stdout. On failure
// stdout is still returned (diff exits 1 when files differ).
func runCommand(ctx context.Context, env []string, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 400 {
			message = message[len(message)-400:]
		}
		err = fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, message)
	}
	return stdout.Bytes(), err
}

// hostRun runs a host binary in the host namespaces of PID 1.
func (ex *executor) hostRun(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	if !ex.hostExec {
		return nil, errors.New("this payload needs the host-namespace executor")
	}
	argv := append([]string{"-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "env", "-i", hostPath}, env...)
	argv = append(argv, name)
	return ex.run(ctx, nil, nil, "nsenter", append(argv, args...)...)
}

// hostStep runs one bounded host command.
func (ex *executor) hostStep(budget time.Duration, env []string, name string, args ...string) ([]byte, error) {
	ctx, cancel, err := ex.step(budget)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return ex.hostRun(ctx, env, name, args...)
}

// restart restarts allowlisted units one by one and requires them active.
func (ex *executor) restart(units []string) error {
	for _, unit := range units {
		if _, err := ex.hostStep(2*time.Minute, nil, "systemctl", "restart", unit); err != nil {
			return err
		}
		out, err := ex.hostStep(10*time.Second, nil, "systemctl", "is-active", unit)
		if err != nil || strings.TrimSpace(string(out)) != "active" {
			return fmt.Errorf("%s is not active after restart: %w", unit, err)
		}
	}
	return nil
}

// ---- node-deb ----

func (ex *executor) debPath(req *request, d deb) string {
	return path.Join(stateDir, req.HotfixID, "debs", d.SHA256+".deb")
}

// stageDebs downloads each deb into the hotfix state directory, or reuses a
// file with the pinned hash.
func (ex *executor) stageDebs(req *request, debs []deb) ([]string, error) {
	if err := os.MkdirAll(ex.host(path.Join(stateDir, req.HotfixID, "debs")), 0o700); err != nil {
		return nil, err
	}
	var paths []string
	for _, d := range debs {
		p := ex.debPath(req, d)
		if data, err := os.ReadFile(ex.host(p)); err != nil || sum(data) != d.SHA256 {
			data, err := ex.fetch(d.URL, d.SHA256)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(ex.host(p), data, 0o600); err != nil {
				return nil, err
			}
		}
		fields, err := ex.hostStep(30*time.Second, nil, "dpkg-deb", "-f", p, "Package", "Version")
		if err != nil {
			return nil, err
		}
		want := fmt.Sprintf("Package: %s\nVersion: %s", d.Package, d.Version)
		if strings.TrimSpace(string(fields)) != want {
			return nil, fmt.Errorf("%s control fields %q do not match %s %s", d.URL, strings.TrimSpace(string(fields)), d.Package, d.Version)
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// installed returns the installed version of each package, "" if absent.
func (ex *executor) installed(debs []deb) (map[string]string, error) {
	versions := map[string]string{}
	for _, d := range debs {
		out, err := ex.hostStep(30*time.Second, nil, "dpkg-query", "-W", "-f=${db:Status-Abbrev} ${Version}", d.Package)
		fields := strings.Fields(string(out))
		// The second status letter is the current state; "i" is installed
		// (also for held packages, "hi").
		if err == nil && len(fields) == 2 && len(fields[0]) >= 2 && fields[0][1] == 'i' {
			versions[d.Package] = fields[1]
		} else {
			versions[d.Package] = ""
		}
	}
	return versions, nil
}

// packageState hashes "package=version" lines in payload order.
func packageState(debs []deb, version func(deb) string) string {
	var b strings.Builder
	for _, d := range debs {
		fmt.Fprintf(&b, "%s=%s\n", d.Package, version(d))
	}
	return sum([]byte(b.String()))
}

func (ex *executor) usrWritable() error {
	out, err := ex.hostStep(10*time.Second, nil, "findmnt", "-n", "-o", "OPTIONS", "--target", "/usr")
	if err != nil {
		return err
	}
	if slices.Contains(strings.Split(strings.TrimSpace(string(out)), ","), "ro") {
		return errors.New("/usr is read-only on this node; deb payloads are not supported here")
	}
	return nil
}

func (ex *executor) debs(req *request) (*result, error) {
	debs := req.Payload.Debs
	current, err := ex.installed(debs)
	if err != nil {
		return nil, err
	}
	before := packageState(debs, func(d deb) string { return current[d.Package] })
	target := packageState(debs, func(d deb) string { return d.Version })
	switch req.Stage {
	case stagePlan:
		var fresh []string
		for _, d := range debs {
			if current[d.Package] == "" {
				fresh = append(fresh, d.Package)
			}
		}
		if before == target {
			return &result{Result: "planned", Action: "skip", BeforeSHA256: before, AfterSHA256: target}, nil
		}
		if err := ex.usrWritable(); err != nil {
			return nil, err
		}
		paths, err := ex.stageDebs(req, debs)
		if err != nil {
			return nil, err
		}
		// dpkg --dry-run resolves dependencies without changing the node.
		if _, err := ex.hostStep(2*time.Minute, nil, "dpkg", append([]string{"--dry-run", "-i"}, paths...)...); err != nil {
			return nil, fmt.Errorf("dependency check failed: %w", err)
		}
		var diff strings.Builder
		for _, d := range debs {
			fmt.Fprintf(&diff, "%s: %q -> %q\n", d.Package, current[d.Package], d.Version)
		}
		return &result{Result: "planned", Action: "apply", BeforeSHA256: before, AfterSHA256: target, Diff: diff.String(), NewPackages: fresh}, nil
	case stageApply:
		if before == req.Expected.AfterSHA256 {
			return &result{Result: "skipped", BeforeSHA256: before, AfterSHA256: before}, nil
		}
		if before != req.Expected.BeforeSHA256 || target != req.Expected.AfterSHA256 {
			return nil, errors.New("installed package versions changed since the plan")
		}
		if err := ex.usrWritable(); err != nil {
			return nil, err
		}
		paths, err := ex.stageDebs(req, debs)
		if err != nil {
			return nil, err
		}
		if err := ex.dpkgInstall(debs, paths); err != nil {
			return nil, err
		}
		if err := ex.verifyDebs(debs, func(d deb) string { return d.Version }); err != nil {
			return nil, err
		}
		if err := ex.restart(req.Payload.Restart); err != nil {
			return nil, err
		}
		return &result{Result: "applied", BeforeSHA256: before, AfterSHA256: target}, nil
	case stageRollback:
		return ex.rollbackDebs(req, before)
	}
	return nil, fmt.Errorf("unsupported stage %q", req.Stage)
}

// dpkgInstall installs debs in one transaction. policy-rc.d stops maintainer
// scripts from (re)starting services; --force-hold keeps held packages
// installable, and their holds are re-asserted afterwards.
func (ex *executor) dpkgInstall(debs []deb, paths []string) error {
	if err := ex.fits(10 * time.Minute); err != nil {
		return err
	}
	held, err := ex.heldPackages(debs)
	if err != nil {
		return err
	}
	restorePolicy, err := ex.blockServiceStarts()
	if err != nil {
		return err
	}
	_, installErr := ex.hostStep(10*time.Minute, []string{"DEBIAN_FRONTEND=noninteractive"}, "dpkg",
		append([]string{"--force-hold", "--force-confold", "-i"}, paths...)...)
	policyErr := restorePolicy()
	holdErr := ex.hold(held)
	if installErr != nil {
		return errors.Join(installErr, policyErr, holdErr)
	}
	if err := errors.Join(policyErr, holdErr); err != nil {
		return err
	}
	out, err := ex.hostStep(time.Minute, nil, "dpkg", "--audit")
	if err != nil || strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("dpkg --audit reports problems after the install: %s %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (ex *executor) heldPackages(debs []deb) ([]string, error) {
	var held []string
	for _, d := range debs {
		out, _ := ex.hostStep(30*time.Second, nil, "dpkg-query", "-W", "-f=${Status}", d.Package)
		if strings.HasPrefix(string(out), "hold ") {
			held = append(held, d.Package)
		}
	}
	return held, nil
}

func (ex *executor) hold(packages []string) error {
	for _, name := range packages {
		if _, err := ex.hostStep(30*time.Second, nil, "apt-mark", "hold", name); err != nil {
			return err
		}
	}
	return nil
}

const policyRC = "/usr/sbin/policy-rc.d"

// blockServiceStarts installs a policy-rc.d that denies every service action
// and returns a function that restores the previous state.
func (ex *executor) blockServiceStarts() (func() error, error) {
	target := ex.host(policyRC)
	previous, readErr := os.ReadFile(target)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, readErr
	}
	if err := os.WriteFile(target, []byte("#!/bin/sh\n# breakglass hotfix: no service starts during dpkg\nexit 101\n"), 0o755); err != nil {
		return nil, err
	}
	return func() error {
		if readErr == nil {
			return os.WriteFile(target, previous, 0o755)
		}
		return os.Remove(target)
	}, nil
}

func (ex *executor) verifyDebs(debs []deb, want func(deb) string) error {
	versions, err := ex.installed(debs)
	if err != nil {
		return err
	}
	for _, d := range debs {
		if versions[d.Package] != want(d) {
			return fmt.Errorf("%s has version %q, want %q", d.Package, versions[d.Package], want(d))
		}
	}
	return nil
}

// rollbackDebs reinstalls rollbackDebs and purges packages the hotfix added.
func (ex *executor) rollbackDebs(req *request, current string) (*result, error) {
	if current == req.Expected.BeforeSHA256 {
		return &result{Result: "skipped", BeforeSHA256: current, AfterSHA256: current}, nil
	}
	if current != req.Expected.AfterSHA256 {
		return nil, errors.New("installed versions changed after the hotfix; not rolling back automatically")
	}
	if len(req.Payload.RollbackDebs) != 0 {
		if err := ex.usrWritable(); err != nil {
			return nil, err
		}
		paths, err := ex.stageDebs(req, req.Payload.RollbackDebs)
		if err != nil {
			return nil, err
		}
		if err := ex.dpkgInstallDowngrade(req.Payload.RollbackDebs, paths); err != nil {
			return nil, err
		}
	}
	for _, name := range req.Expected.NewPackages {
		if err := ex.fits(5 * time.Minute); err != nil {
			return nil, err
		}
		restorePolicy, err := ex.blockServiceStarts()
		if err != nil {
			return nil, err
		}
		_, purgeErr := ex.hostStep(5*time.Minute, []string{"DEBIAN_FRONTEND=noninteractive"}, "dpkg", "--purge", name)
		if err := errors.Join(purgeErr, restorePolicy()); err != nil {
			return nil, err
		}
	}
	if err := ex.restart(req.Payload.Restart); err != nil {
		return nil, err
	}
	return &result{Result: "rolledBack", BeforeSHA256: current, AfterSHA256: req.Expected.BeforeSHA256}, nil
}

func (ex *executor) dpkgInstallDowngrade(debs []deb, paths []string) error {
	if err := ex.fits(10 * time.Minute); err != nil {
		return err
	}
	held, err := ex.heldPackages(debs)
	if err != nil {
		return err
	}
	restorePolicy, err := ex.blockServiceStarts()
	if err != nil {
		return err
	}
	_, installErr := ex.hostStep(10*time.Minute, []string{"DEBIAN_FRONTEND=noninteractive"}, "dpkg",
		append([]string{"--force-hold", "--force-confold", "--force-downgrade", "-i"}, paths...)...)
	if err := errors.Join(installErr, restorePolicy(), ex.hold(held)); err != nil {
		return err
	}
	return ex.verifyDebs(debs, func(d deb) string { return d.Version })
}

// ---- static-pod-image ----

var imageLine = regexp.MustCompile(`(?m)^(\s*(?:- )?)image:\s*\S+\s*$`)

func manifestPath(pod string) string { return "/etc/kubernetes/manifests/" + pod + ".yaml" }

// patchImage replaces the single image line of a kubeadm static Pod manifest.
func patchImage(manifest []byte, image string) ([]byte, error) {
	matches := imageLine.FindAllSubmatchIndex(manifest, -1)
	if len(matches) != 1 {
		return nil, fmt.Errorf("manifest has %d image lines; exactly one is supported", len(matches))
	}
	m := matches[0]
	var out bytes.Buffer
	out.Write(manifest[:m[0]])
	out.Write(manifest[m[2]:m[3]])
	out.WriteString("image: " + image)
	out.Write(manifest[m[1]:])
	return out.Bytes(), nil
}

type crictlContainers struct {
	Containers []struct {
		ID       string `json:"id"`
		ImageRef string `json:"imageRef"`
	} `json:"containers"`
}

// imageID pulls nothing; it returns the local image ID of a pulled image.
func (ex *executor) imageID(image string) (string, error) {
	out, err := ex.hostStep(30*time.Second, nil, "crictl", "inspecti", "-o", "json", image)
	if err != nil {
		return "", err
	}
	var inspected struct {
		Status struct {
			ID          string   `json:"id"`
			RepoDigests []string `json:"repoDigests"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &inspected); err != nil || inspected.Status.ID == "" {
		return "", fmt.Errorf("inspect %s: unexpected output", image)
	}
	digest := image[strings.LastIndex(image, "@"):]
	if !slices.ContainsFunc(inspected.Status.RepoDigests, func(d string) bool { return strings.HasSuffix(d, digest) }) {
		return "", fmt.Errorf("pulled image does not carry digest %s", digest)
	}
	return inspected.Status.ID, nil
}

// waitStaticPod waits until exactly the static Pod's container runs the image ID.
func (ex *executor) waitStaticPod(pod, id string, budget time.Duration) error {
	if err := ex.fits(budget); err != nil {
		return err
	}
	end := ex.now().Add(budget)
	for {
		out, err := ex.hostStep(15*time.Second, nil, "crictl", "ps", "--name", "^"+pod+"$", "--state", "running", "-o", "json")
		var running crictlContainers
		if err == nil && json.Unmarshal(out, &running) == nil && len(running.Containers) == 1 && running.Containers[0].ImageRef == id {
			return nil
		}
		if ex.now().After(end) {
			return fmt.Errorf("%s did not run image %s within %s", pod, id, budget)
		}
		time.Sleep(5 * time.Second)
	}
}

type etcdStatus struct {
	Endpoint string `json:"Endpoint"`
	Status   struct {
		Leader uint64 `json:"leader"`
	} `json:"Status"`
}

// etcdHealthy requires every member to answer and to agree on one leader.
// Changing one member at a time then keeps quorum.
func (ex *executor) etcdHealthy() error {
	out, err := ex.hostStep(15*time.Second, nil, "crictl", "ps", "--name", "^etcd$", "--state", "running", "-q")
	ids := strings.Fields(string(out))
	if err != nil || len(ids) != 1 {
		return fmt.Errorf("local etcd container is not running: %w", err)
	}
	etcdctl := []string{"exec", ids[0], "etcdctl", "--endpoints=https://127.0.0.1:2379",
		"--cacert=/etc/kubernetes/pki/etcd/ca.crt", "--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt",
		"--key=/etc/kubernetes/pki/etcd/healthcheck-client.key", "-w", "json"}
	members, err := ex.hostStep(30*time.Second, nil, "crictl", append(etcdctl, "member", "list")...)
	if err != nil {
		return err
	}
	var list struct {
		Members []json.RawMessage `json:"members"`
	}
	if err := json.Unmarshal(members, &list); err != nil || len(list.Members) == 0 {
		return errors.New("etcd member list is unreadable")
	}
	out, err = ex.hostStep(30*time.Second, nil, "crictl", append(etcdctl, "endpoint", "status", "--cluster")...)
	if err != nil {
		return fmt.Errorf("etcd endpoint status: %w", err)
	}
	var statuses []etcdStatus
	if err := json.Unmarshal(out, &statuses); err != nil || len(statuses) != len(list.Members) {
		return fmt.Errorf("only %d of %d etcd members answer", len(statuses), len(list.Members))
	}
	for _, s := range statuses {
		if s.Status.Leader == 0 || s.Status.Leader != statuses[0].Status.Leader {
			return errors.New("etcd members do not agree on one leader")
		}
	}
	return nil
}

// waitEtcdHealthy polls etcdHealthy within the budget.
func (ex *executor) waitEtcdHealthy(budget time.Duration) error {
	end := ex.now().Add(budget)
	for {
		err := ex.etcdHealthy()
		if err == nil {
			return nil
		}
		if ex.now().After(end) {
			return err
		}
		time.Sleep(5 * time.Second)
	}
}

func (ex *executor) staticPod(req *request) (*result, error) {
	pod := req.Payload.Image.StaticPod
	p := manifestPath(pod)
	old, info, err := ex.read(p)
	if err != nil {
		return nil, err
	}
	before := sum(old)
	updated, err := patchImage(old, req.Payload.Image.Image)
	if err != nil {
		return nil, err
	}
	after := sum(updated)
	switch req.Stage {
	case stagePlan:
		action := "apply"
		if before == after {
			action = "skip"
		}
		return &result{Result: "planned", Action: action, BeforeSHA256: before, AfterSHA256: after, Diff: ex.reviewDiff(p, old, updated)}, nil
	case stageApply:
		if before == req.Expected.AfterSHA256 {
			return &result{Result: "skipped", BeforeSHA256: before, AfterSHA256: before}, nil
		}
		if before != req.Expected.BeforeSHA256 || after != req.Expected.AfterSHA256 {
			return nil, fmt.Errorf("%s changed since the plan", p)
		}
		if pod == "etcd" {
			if err := ex.etcdHealthy(); err != nil {
				return nil, fmt.Errorf("refusing to restart an etcd member without full quorum: %w", err)
			}
		}
		// Pre-pull, so the Pod restarts without waiting for the registry.
		if _, err := ex.hostStep(5*time.Minute, nil, "crictl", "pull", req.Payload.Image.Image); err != nil {
			return nil, err
		}
		id, err := ex.imageID(req.Payload.Image.Image)
		if err != nil {
			return nil, err
		}
		if err := ex.fits(6 * time.Minute); err != nil {
			return nil, err
		}
		if err := ex.replaceFile(req, p, old, updated, info); err != nil {
			return nil, err
		}
		if err := ex.waitStaticPod(pod, id, 5*time.Minute); err != nil {
			return nil, err
		}
		if pod == "etcd" {
			if err := ex.waitEtcdHealthy(time.Minute); err != nil {
				return nil, err
			}
		}
		return &result{Result: "applied", BeforeSHA256: before, AfterSHA256: after}, nil
	case stageRollback:
		out, err := ex.restoreFile(req, p, before, info, nil)
		if err != nil || out.Result == "skipped" {
			return out, err
		}
		if pod == "etcd" {
			if err := ex.waitEtcdHealthy(5 * time.Minute); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported stage %q", req.Stage)
}

// ---- host-container-image ----

func (ex *executor) hostContainer(req *request) (*result, error) {
	c := req.HostContainer
	old, info, err := ex.read(c.ImageRefFile)
	if err != nil {
		return nil, err
	}
	before := sum(old)
	updated := []byte(req.Payload.Image.Image + "\n")
	after := sum(updated)
	switch req.Stage {
	case stagePlan:
		if _, err := ex.hostStep(15*time.Second, nil, "systemctl", "cat", c.Unit); err != nil {
			return nil, fmt.Errorf("unit %s is not installed: %w", c.Unit, err)
		}
		action := "apply"
		if before == after {
			action = "skip"
		}
		return &result{Result: "planned", Action: action, BeforeSHA256: before, AfterSHA256: after, Diff: ex.reviewDiff(c.ImageRefFile, old, updated)}, nil
	case stageApply:
		if before == req.Expected.AfterSHA256 {
			return &result{Result: "skipped", BeforeSHA256: before, AfterSHA256: before}, nil
		}
		if before != req.Expected.BeforeSHA256 || after != req.Expected.AfterSHA256 {
			return nil, fmt.Errorf("%s changed since the plan", c.ImageRefFile)
		}
		if _, err := ex.hostStep(5*time.Minute, nil, "ctr", "-n", c.ContainerdNamespace, "images", "pull",
			"--hosts-dir", "/etc/containerd/certs.d", req.Payload.Image.Image); err != nil {
			return nil, err
		}
		if err := ex.fits(4 * time.Minute); err != nil {
			return nil, err
		}
		if err := ex.replaceFile(req, c.ImageRefFile, old, updated, info); err != nil {
			return nil, err
		}
		if err := ex.restartHostContainer(c, req.Payload.Image.Image); err != nil {
			return nil, err
		}
		return &result{Result: "applied", BeforeSHA256: before, AfterSHA256: after}, nil
	case stageRollback:
		out, err := ex.restoreFile(req, c.ImageRefFile, before, info, nil)
		if err != nil || out.Result == "skipped" {
			return out, err
		}
		saved, err := os.ReadFile(ex.host(c.ImageRefFile))
		if err != nil {
			return nil, err
		}
		if err := ex.restartHostContainer(c, strings.TrimSpace(string(saved))); err != nil {
			return nil, err
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported stage %q", req.Stage)
}

// restartHostContainer restarts the unit and requires the container to run
// the expected image. Restarting cra-vsr interrupts the node's network.
func (ex *executor) restartHostContainer(c *hostContainer, image string) error {
	if _, err := ex.hostStep(3*time.Minute, nil, "systemctl", "restart", c.Unit); err != nil {
		return err
	}
	out, err := ex.hostStep(30*time.Second, nil, "nerdctl", "--namespace", c.ContainerdNamespace, "inspect",
		"--format", "{{.Image}} {{.State.Running}}", c.Name)
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != image+" true" {
		return fmt.Errorf("%s runs %q after the restart, want %q", c.Name, got, image+" true")
	}
	return nil
}
