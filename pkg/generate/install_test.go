package generate

import (
	"reflect"
	"strings"
	"testing"

	"github.com/blang/semver/v4"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const installManifest = `apiVersion: cluster.x-k8s.io/v1beta2
kind: Cluster
metadata:
  name: demo
---
apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: KubeadmControlPlane
metadata:
  name: demo-control-plane
spec:
  version: v1.33.4
  kubeadmConfigSpec:
    files:
    - path: /etc/kube-vip-prepare.sh
      owner: root:root
      permissions: "0700"
      content: "#!/bin/bash\n"
    preKubeadmCommands:
    - /etc/kube-vip-prepare.sh
    users:
    - name: root
---
apiVersion: cluster.x-k8s.io/v1beta2
kind: MachineDeployment
metadata:
  name: demo-workers
spec:
  template:
    spec:
      version: v1.33.4
---
apiVersion: bootstrap.cluster.x-k8s.io/v1beta2
kind: KubeadmConfigTemplate
metadata:
  name: demo-worker
spec:
  template:
    spec:
      joinConfiguration:
        nodeRegistration: {}
`

func TestSetInstallKubernetes(t *testing.T) {
	b := mustParse(t, installManifest)
	opts := InstallKubernetesOptions{
		AptProxy:        "http://cache.lan:3142",
		RegistryMirrors: map[string][]string{"registry.k8s.io": {"http://cache.lan:5000"}, "docker.io": {"http://cache.lan:5001", "https://mirror.lan"}},
	}
	for range 2 {
		if err := SetInstallKubernetes(b, opts); err != nil {
			t.Fatal(err)
		}
	}
	assertInSync(t, b)

	kcp := b.Objects[1]
	kct := b.Objects[3]
	for _, tc := range []struct {
		obj  *unstructured.Unstructured
		path []string
		pre  []string
		n    int
	}{
		{kcp, []string{"spec", "kubeadmConfigSpec"}, []string{InstallScriptPath, "/etc/kube-vip-prepare.sh"}, 2},
		{kct, []string{"spec", "template", "spec"}, []string{InstallScriptPath}, 1},
	} {
		files, _, _ := unstructured.NestedSlice(tc.obj.Object, append(tc.path, "files")...)
		if len(files) != tc.n {
			t.Fatalf("%s files = %v", tc.obj.GetKind(), files)
		}
		f := files[0].(map[string]any)
		if f["path"] != InstallScriptPath || f["permissions"] != "0755" || f["owner"] != "root:root" {
			t.Errorf("%s file = %v", tc.obj.GetKind(), f)
		}
		content, _ := f["content"].(string)
		for _, want := range []string{
			"#!/usr/bin/env bash", "set -euo pipefail", `WANT_VERSION="1.33.4"`, `WANT_MINOR="v1.33"`,
			`APT_PROXY="http://cache.lan:3142"`, "/etc/apt/apt.conf.d/01klotho-proxy",
			"core:/stable:/$WANT_MINOR/deb", `"kubeadm=${WANT_VERSION}-*"`, "apt-mark hold kubelet kubeadm kubectl",
			"SystemdCgroup", "br_netfilter", "net.ipv4.ip_forward", "swapoff -a", InstallLogPath,
			`config_path\s*=\s*).*$#\1"/etc/containerd/certs.d"`,
			"/etc/containerd/certs.d/registry.k8s.io/hosts.toml", `server = "https://registry.k8s.io"`,
			`[host."http://cache.lan:5000"]`, `capabilities = ["pull", "resolve"]`,
			`server = "https://registry-1.docker.io"`, `[host."https://mirror.lan"]`,
		} {
			if !strings.Contains(content, want) {
				t.Errorf("%s script lacks %q", tc.obj.GetKind(), want)
			}
		}
		// Mirrors render in a fixed order.
		if strings.Index(content, "certs.d/docker.io") > strings.Index(content, "certs.d/registry.k8s.io") {
			t.Error("mirrors not sorted")
		}
		pre, _, _ := unstructured.NestedStringSlice(tc.obj.Object, append(tc.path, "preKubeadmCommands")...)
		if !reflect.DeepEqual(pre, tc.pre) {
			t.Errorf("%s preKubeadmCommands = %v, want %v", tc.obj.GetKind(), pre, tc.pre)
		}
	}
	if users, _, _ := unstructured.NestedSlice(kcp.Object, "spec", "kubeadmConfigSpec", "users"); len(users) != 1 {
		t.Error("sibling field dropped")
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(kct.Object, "spec", "template", "spec", "joinConfiguration"); !ok {
		t.Error("worker sibling field dropped")
	}
	want := "    preKubeadmCommands:\n    - " + InstallScriptPath + "\n    - /etc/kube-vip-prepare.sh\n"
	if !strings.Contains(string(b.YAML), want) {
		t.Errorf("YAML lacks the command order:\n%s", b.YAML)
	}
}

func TestSetInstallKubernetesWithoutOptions(t *testing.T) {
	b := mustParse(t, installManifest)
	if err := SetInstallKubernetes(b, InstallKubernetesOptions{}); err != nil {
		t.Fatal(err)
	}
	files, _, _ := unstructured.NestedSlice(b.Objects[1].Object, "spec", "kubeadmConfigSpec", "files")
	content := files[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, `APT_PROXY=""`) || strings.Contains(content, "certs.d") {
		t.Errorf("options leaked into the script:\n%s", content)
	}
}

func TestSetInstallKubernetesErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(string) string
		opts   InstallKubernetesOptions
		want   string
	}{
		"no control plane": {
			mutate: func(s string) string { return strings.Replace(s, "kind: KubeadmControlPlane", "kind: Other", 1) },
			want:   "no controlplane.cluster.x-k8s.io object",
		},
		"no version": {
			mutate: func(s string) string { return strings.Replace(s, "  version: v1.33.4\n  kubeadm", "  kubeadm", 1) },
			want:   "spec.version: is empty",
		},
		"bad version": {
			mutate: func(s string) string {
				return strings.Replace(s, "  version: v1.33.4\n  kubeadm", "  version: latest\n  kubeadm", 1)
			},
			want: `"latest" is not a Kubernetes version`,
		},
		"worker version differs": {
			mutate: func(s string) string {
				return strings.Replace(s, "      version: v1.33.4", "      version: v1.32.0", 1)
			},
			want: `MachineDeployment "demo-workers" asks for Kubernetes v1.32.0`,
		},
		"bad apt proxy": {
			mutate: func(s string) string { return s },
			opts:   InstallKubernetesOptions{AptProxy: "cache.lan:3142"},
			want:   "apt proxy",
		},
		"bad mirror": {
			mutate: func(s string) string { return s },
			opts:   InstallKubernetesOptions{RegistryMirrors: map[string][]string{"docker.io": {"ftp://x"}}},
			want:   "registry docker.io mirror",
		},
		"empty mirrors": {
			mutate: func(s string) string { return s },
			opts:   InstallKubernetesOptions{RegistryMirrors: map[string][]string{"docker.io": nil}},
			want:   "lists no mirrors",
		},
		"bad registry": {
			mutate: func(s string) string { return s },
			opts:   InstallKubernetesOptions{RegistryMirrors: map[string][]string{"docker.io/library": {"http://x"}}},
			want:   "not a registry host",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := SetInstallKubernetes(mustParse(t, tc.mutate(installManifest)), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestInstallScriptIsBash(t *testing.T) {
	script, err := InstallScript(semver.MustParse("1.33.4"), InstallKubernetesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := string(script)
	if !strings.HasPrefix(s, "#!/usr/bin/env bash\n") || strings.Contains(s, "{{") {
		t.Errorf("script:\n%s", s)
	}
	// Every heredoc is closed.
	if strings.Count(s, "<<'EOF'")+strings.Count(s, "<<EOF") != strings.Count(s, "\nEOF\n") {
		t.Errorf("unbalanced heredocs:\n%s", s)
	}
}

func TestInstallScriptProviderPackages(t *testing.T) {
	v := semver.MustParse("1.33.4")
	s, err := InstallScript(v, InstallKubernetesOptions{Packages: []string{"qemu-guest-agent"}, AptProxy: "http://cache.lan:3142"})
	if err != nil {
		t.Fatal(err)
	}
	script := string(s)
	pkg := strings.Index(script, "for p in qemu-guest-agent; do")
	proxy := strings.Index(script, "01klotho-proxy")
	early := strings.Index(script, "already installed; nothing to do")
	if pkg < 0 || proxy < 0 || early < 0 || !(proxy < pkg && pkg < early) {
		t.Fatalf("order proxy=%d packages=%d early-exit=%d:\n%s", proxy, pkg, early, script)
	}
	if !strings.Contains(script, `systemctl enable --now "$p"`) {
		t.Fatalf("package service not enabled:\n%s", script)
	}
	s, err = InstallScript(v, InstallKubernetesOptions{})
	if err != nil || strings.Contains(string(s), "provider package") {
		t.Fatalf("packages block rendered without packages (err %v)", err)
	}
}
