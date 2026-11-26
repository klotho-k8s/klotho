package kubeadm

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/blang/semver/v4"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"

	bootstrapv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
	controlplanev1 "sigs.k8s.io/cluster-api/api/controlplane/kubeadm/v1beta2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	kubeadmtypes "sigs.k8s.io/cluster-api/bootstrap/kubeadm/types"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/collections"
	"sigs.k8s.io/cluster-api/util/secret"

	"github.com/klotho-k8s/klotho/pkg/cloudinit"
	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/provider/controlplane"
)

const (
	testVersion = "v1.33.4"
	testVIP     = "10.0.0.10"
	testPort    = 6443
)

// testKCP is a KubeadmControlPlane shaped like the Proxmox provider's
// template after variable substitution: a static pod file, a pre-kubeadm
// script, a root user, and a Jinja provider-id. It adds a certSAN, an API
// server argument, a post-kubeadm command with Jinja, and a non-root user to
// cover the rest of the spec handling. Networking is not part of a v1beta2
// ClusterConfiguration; it comes from Options.
const testKCP = `apiVersion: controlplane.cluster.x-k8s.io/v1beta2
kind: KubeadmControlPlane
metadata:
  name: demo-control-plane
  namespace: capi
spec:
  replicas: 1
  version: v1.33.4
  machineTemplate:
    metadata:
      labels:
        team: infra
    spec:
      infrastructureRef:
        apiGroup: infrastructure.cluster.x-k8s.io
        kind: ProxmoxMachineTemplate
        name: demo-control-plane
  kubeadmConfigSpec:
    users:
    - name: root
      sshAuthorizedKeys:
      - ssh-ed25519 AAAAtemplate root@template
    - name: ops
      sudo: ALL=(ALL) NOPASSWD:ALL
      sshAuthorizedKeys:
      - ssh-ed25519 AAAAops ops@template
    files:
    - path: /etc/kubernetes/manifests/kube-vip.yaml
      owner: root:root
      content: |
        apiVersion: v1
        kind: Pod
        metadata:
          name: kube-vip
          namespace: kube-system
        spec:
          containers:
          - name: kube-vip
            image: ghcr.io/kube-vip/kube-vip:v0.7.1
            env:
            - name: address
              value: 10.0.0.10
          hostNetwork: true
    - path: /etc/kube-vip-prepare.sh
      owner: root:root
      permissions: "0700"
      content: |
        #!/bin/bash
        sed -i 's#path: /etc/kubernetes/admin.conf#path: /etc/kubernetes/super-admin.conf#' /etc/kubernetes/manifests/kube-vip.yaml
    - path: /etc/node-identity
      content: "node={{ ds.meta_data.local_hostname }}\n"
    - path: /etc/encoded
      encoding: base64
      content: aGVsbG8K
    preKubeadmCommands:
    - /etc/kube-vip-prepare.sh
    - echo "{{ ds.meta_data.hostname }}" > /etc/klotho-test-hostname
    postKubeadmCommands:
    - echo done
    clusterConfiguration:
      apiServer:
        certSANs:
        - api.example.test
        extraArgs:
        - name: audit-log-maxage
          value: "7"
    initConfiguration:
      nodeRegistration:
        kubeletExtraArgs:
        - name: provider-id
          value: "proxmox://'{{ ds.meta_data.instance_id }}'"
    joinConfiguration:
      nodeRegistration:
        kubeletExtraArgs:
        - name: provider-id
          value: "proxmox://'{{ ds.meta_data.instance_id }}'"
`

func controlPlaneObject(t *testing.T, doc string) *unstructured.Unstructured {
	t.Helper()
	obj := map[string]any{}
	if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: obj}
}

func typedKCP(t *testing.T, u *unstructured.Unstructured) *controlplanev1.KubeadmControlPlane {
	t.Helper()
	kcp, err := decodeControlPlane(u)
	if err != nil {
		t.Fatal(err)
	}
	return kcp
}

func testSpec() *cluster.Spec {
	return &cluster.Spec{Name: "demo", Namespace: "capi", Provider: "proxmox", ControlPlane: Name}
}

func testProvider() *Provider {
	return New(Options{
		PodCIDR:           "10.244.0.0/16",
		ServiceCIDR:       "10.96.0.0/12",
		SSHAuthorizedKeys: []string{"ssh-ed25519 AAAAexample user@host"},
		ExtraPostCommands: []string{"echo extra-step"},
	})
}

func prepare(t *testing.T, p *Provider) *controlplane.Credentials {
	t.Helper()
	creds, err := p.Prepare(context.Background(), testSpec(), cluster.Endpoint{Host: testVIP, Port: testPort})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return creds
}

func TestNameAndKind(t *testing.T) {
	p := New(Options{})
	if p.Name() != "kubeadm" || p.Kind() != "KubeadmControlPlane" {
		t.Fatalf("Name() = %q, Kind() = %q", p.Name(), p.Kind())
	}
}

func TestPrepare(t *testing.T) {
	creds := prepare(t, testProvider())

	if len(creds.CA.Cert) == 0 || len(creds.CA.Key) == 0 {
		t.Fatal("cluster CA missing")
	}
	for _, purpose := range []secret.Purpose{secret.EtcdCA, secret.FrontProxyCA, secret.ServiceAccount} {
		kp, ok := creds.Extra[string(purpose)]
		if !ok || len(kp.Cert) == 0 || len(kp.Key) == 0 {
			t.Errorf("Extra[%q] missing or empty", purpose)
		}
	}
	if len(creds.Extra) != 3 {
		t.Errorf("Extra has %d entries, want 3", len(creds.Extra))
	}

	cfg, err := clientcmd.Load(creds.Kubeconfig)
	if err != nil {
		t.Fatalf("kubeconfig does not parse: %v", err)
	}
	ctx := cfg.Contexts[cfg.CurrentContext]
	if ctx == nil {
		t.Fatalf("current context %q missing", cfg.CurrentContext)
	}
	cl := cfg.Clusters[ctx.Cluster]
	if cl == nil {
		t.Fatalf("cluster %q missing", ctx.Cluster)
	}
	if want := "https://10.0.0.10:6443"; cl.Server != want {
		t.Errorf("server = %q, want %q", cl.Server, want)
	}
	if !bytes.Equal(cl.CertificateAuthorityData, creds.CA.Cert) {
		t.Error("kubeconfig CA differs from Credentials.CA.Cert")
	}

	ca := parseCert(t, creds.CA.Cert)
	if !ca.IsCA {
		t.Error("cluster CA is not a CA")
	}
	user := cfg.AuthInfos[ctx.AuthInfo]
	if user == nil {
		t.Fatalf("user %q missing", ctx.AuthInfo)
	}
	client := parseCert(t, user.ClientCertificateData)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := client.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("client certificate does not verify against the CA: %v", err)
	}
	if !slices.Contains(client.Subject.Organization, "system:masters") {
		t.Errorf("client certificate organizations = %v", client.Subject.Organization)
	}
}

func TestPrepareRejectsMissingInputs(t *testing.T) {
	p := testProvider()
	if _, err := p.Prepare(context.Background(), &cluster.Spec{}, cluster.Endpoint{Host: testVIP}); err == nil {
		t.Error("expected an error without a cluster name")
	}
	if _, err := p.Prepare(context.Background(), testSpec(), cluster.Endpoint{}); err == nil {
		t.Error("expected an error without an endpoint host")
	}
}

func TestEndpointFixScript(t *testing.T) {
	p := testProvider()
	creds := prepare(t, p)
	cp := controlPlaneObject(t, testKCP)
	c, err := p.UserData(context.Background(), testSpec(), creds, cp, nil)
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	var fix *controlplane.File
	for i := range c.Files {
		if c.Files[i].Path == EndpointFixScript {
			fix = &c.Files[i]
		}
	}
	if fix == nil {
		t.Fatalf("no %s among the files", EndpointFixScript)
	}
	if fix.Mode != "0700" || fix.Owner != "root:root" {
		t.Errorf("endpoint-fix file = mode %s owner %s", fix.Mode, fix.Owner)
	}
	body := string(fix.Content)
	for _, want := range []string{
		"controlPlaneEndpoint:",   // reads the configured endpoint
		"ip -4 -o addr show",      // tests whether the endpoint is local
		"ip -4 route get 1.1.1.1", // discovers this node's primary IPv4
		"sed -i -E",               // rewrites the config in place
	} {
		if !strings.Contains(body, want) {
			t.Errorf("endpoint-fix script lacks %q", want)
		}
	}
	// It must run before kubeadm init.
	if len(c.PreCommands) == 0 || c.PreCommands[0] != "/bin/bash "+EndpointFixScript {
		t.Errorf("endpoint-fix must be the first pre-kubeadm command: %v", c.PreCommands)
	}
}

func TestUserData(t *testing.T) {
	p := testProvider()
	creds := prepare(t, p)
	cp := controlPlaneObject(t, testKCP)
	c, err := p.UserData(context.Background(), testSpec(), creds, cp, nil)
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	files := map[string]controlplane.File{}
	for _, f := range c.Files {
		if _, dup := files[f.Path]; dup && !f.Append {
			t.Errorf("duplicate file %s", f.Path)
		}
		files[f.Path] = f
	}
	kcp := typedKCP(t, cp)
	specFiles := map[string]bootstrapv1.File{}
	for _, f := range kcp.Spec.KubeadmConfigSpec.Files {
		specFiles[f.Path] = f
	}

	t.Run("pki", func(t *testing.T) {
		want := map[string][]byte{
			"/etc/kubernetes/pki/ca.crt":             creds.CA.Cert,
			"/etc/kubernetes/pki/ca.key":             creds.CA.Key,
			"/etc/kubernetes/pki/etcd/ca.crt":        creds.Extra["etcd"].Cert,
			"/etc/kubernetes/pki/etcd/ca.key":        creds.Extra["etcd"].Key,
			"/etc/kubernetes/pki/front-proxy-ca.crt": creds.Extra["proxy"].Cert,
			"/etc/kubernetes/pki/front-proxy-ca.key": creds.Extra["proxy"].Key,
			"/etc/kubernetes/pki/sa.pub":             creds.Extra["sa"].Cert,
			"/etc/kubernetes/pki/sa.key":             creds.Extra["sa"].Key,
		}
		for p, content := range want {
			f, ok := files[p]
			if !ok {
				t.Errorf("%s missing", p)
				continue
			}
			if !bytes.Equal(f.Content, content) {
				t.Errorf("%s content differs from credentials", p)
			}
			wantMode := "0640"
			if strings.HasSuffix(p, ".key") {
				wantMode = "0600"
			}
			if f.Mode != wantMode || f.Owner != "root:root" {
				t.Errorf("%s mode = %s owner = %s, want %s root:root", p, f.Mode, f.Owner, wantMode)
			}
		}
	})

	t.Run("spec files", func(t *testing.T) {
		for _, p := range []string{"/etc/kubernetes/manifests/kube-vip.yaml", "/etc/kube-vip-prepare.sh"} {
			f, ok := files[p]
			if !ok {
				t.Fatalf("%s missing", p)
			}
			if string(f.Content) != specFiles[p].Content {
				t.Errorf("%s content differs from the spec", p)
			}
			if f.Mode != specFiles[p].Permissions || f.Owner != specFiles[p].Owner {
				t.Errorf("%s mode/owner = %s/%s", p, f.Mode, f.Owner)
			}
		}
		var pod corev1.Pod
		if err := yaml.UnmarshalStrict(files["/etc/kubernetes/manifests/kube-vip.yaml"].Content, &pod); err != nil {
			t.Errorf("kube-vip manifest does not parse: %v", err)
		}
		if got := string(files["/etc/encoded"].Content); got != "hello\n" {
			t.Errorf("base64 file decoded to %q", got)
		}
		if got := string(files["/etc/node-identity"].Content); got != "node=@@KLOTHO_JINJA_LOCAL_HOSTNAME@@\n" {
			t.Errorf("Jinja file rendered to %q", got)
		}
	})

	t.Run("kubeadm config", func(t *testing.T) {
		f, ok := files[KubeadmConfigPath]
		if !ok {
			t.Fatal("kubeadm config missing")
		}
		docs := splitDocs(string(f.Content))
		if len(docs) != 2 {
			t.Fatalf("kubeadm config has %d documents, want 2", len(docs))
		}
		if !strings.Contains(docs[0], "apiVersion: kubeadm.k8s.io/v1beta4") || !strings.Contains(docs[0], "kind: ClusterConfiguration") {
			t.Errorf("first document is not a v1beta4 ClusterConfiguration:\n%s", docs[0])
		}
		cc, data, err := kubeadmtypes.UnmarshalClusterConfiguration(docs[0])
		if err != nil {
			t.Fatalf("ClusterConfiguration does not decode: %v", err)
		}
		if cc.ControlPlaneEndpoint != "10.0.0.10:6443" {
			t.Errorf("controlPlaneEndpoint = %q", cc.ControlPlaneEndpoint)
		}
		if !reflect.DeepEqual(cc.APIServer.CertSANs, kcp.Spec.KubeadmConfigSpec.ClusterConfiguration.APIServer.CertSANs) {
			t.Errorf("certSANs = %v", cc.APIServer.CertSANs)
		}
		if !reflect.DeepEqual(cc.APIServer.ExtraArgs, kcp.Spec.KubeadmConfigSpec.ClusterConfiguration.APIServer.ExtraArgs) {
			t.Errorf("apiServer.extraArgs = %+v", cc.APIServer.ExtraArgs)
		}
		if !cc.FeatureGates[controlPlaneKubeletLocalMode] {
			t.Errorf("featureGates = %v, want %s defaulted as KubeadmControlPlane does", cc.FeatureGates, controlPlaneKubeletLocalMode)
		}
		if deref(data.KubernetesVersion) != testVersion || deref(data.ClusterName) != "demo" ||
			deref(data.PodSubnet) != "10.244.0.0/16" || deref(data.ServiceSubnet) != "10.96.0.0/12" {
			t.Errorf("additional data = version %q name %q pods %q services %q",
				deref(data.KubernetesVersion), deref(data.ClusterName), deref(data.PodSubnet), deref(data.ServiceSubnet))
		}

		ic, err := kubeadmtypes.UnmarshalInitConfiguration(docs[1])
		if err != nil {
			t.Fatalf("InitConfiguration does not decode: %v", err)
		}
		args := ic.NodeRegistration.KubeletExtraArgs
		if len(args) != 1 || args[0].Name != "provider-id" || deref(args[0].Value) != "proxmox://'@@KLOTHO_JINJA_INSTANCE_ID@@'" {
			t.Errorf("kubeletExtraArgs = %+v", args)
		}
	})

	t.Run("jinja", func(t *testing.T) {
		script := string(files[JinjaScript].Content)
		for _, want := range []string{
			"value ds.meta_data.instance_id", "value ds.meta_data.hostname", "value ds.meta_data.local_hostname",
			shellQuote(KubeadmConfigPath), shellQuote("/etc/node-identity"), shellQuote(CommandsDir + "/pre-01.sh"),
			"touch " + JinjaDoneMarker,
		} {
			if !strings.Contains(script, want) {
				t.Errorf("render script lacks %q", want)
			}
		}
		if strings.Contains(script, shellQuote("/etc/kube-vip-prepare.sh")) {
			t.Error("render script rewrites a file without Jinja")
		}
		cmd := string(files[CommandsDir+"/pre-01.sh"].Content)
		if !strings.Contains(cmd, `echo "@@KLOTHO_JINJA_HOSTNAME@@" > /etc/klotho-test-hostname`) || !strings.Contains(cmd, JinjaDoneMarker) {
			t.Errorf("wrapped command = %q", cmd)
		}
	})

	t.Run("secrets", func(t *testing.T) {
		wantSecrets := map[string]string{
			"demo-ca":         secret.TLSCrtDataName,
			"demo-etcd":       secret.TLSCrtDataName,
			"demo-sa":         secret.TLSCrtDataName,
			"demo-proxy":      secret.TLSCrtDataName,
			"demo-kubeconfig": secret.KubeconfigDataName,
		}
		seen := 0
		for p, f := range files {
			if path.Dir(p) != ManifestsDir {
				continue
			}
			if f.Mode != "0600" {
				t.Errorf("%s mode = %s", p, f.Mode)
			}
			for _, doc := range splitDocs(string(f.Content)) {
				if strings.Contains(doc, "kind: Namespace\n") {
					var ns corev1.Namespace
					mustStrict(t, doc, &ns)
					if ns.Name != "capi" {
						t.Errorf("namespace = %q", ns.Name)
					}
					continue
				}
				var s corev1.Secret
				mustStrict(t, doc, &s)
				key, ok := wantSecrets[s.Name]
				if !ok {
					t.Errorf("unexpected secret %q", s.Name)
					continue
				}
				seen++
				if s.Kind != "Secret" || s.APIVersion != "v1" || s.Namespace != "capi" {
					t.Errorf("%s: kind=%s apiVersion=%s namespace=%s", s.Name, s.Kind, s.APIVersion, s.Namespace)
				}
				if s.Type != clusterv1.ClusterSecretType || s.Labels[clusterv1.ClusterNameLabel] != "demo" || len(s.OwnerReferences) != 0 {
					t.Errorf("%s: type=%q labels=%v owners=%v", s.Name, s.Type, s.Labels, s.OwnerReferences)
				}
				if len(s.Data[key]) == 0 {
					t.Errorf("%s has no %q", s.Name, key)
				}
			}
		}
		if seen != len(wantSecrets) {
			t.Errorf("found %d secrets, want %d", seen, len(wantSecrets))
		}
	})

	t.Run("users", func(t *testing.T) {
		f := files["/root/.ssh/authorized_keys"]
		if !f.Append || f.Mode != "0600" ||
			!strings.Contains(string(f.Content), "AAAAtemplate") || !strings.Contains(string(f.Content), "AAAAexample") {
			t.Errorf("root authorized_keys = %+v", f)
		}
		script := string(files[UsersScript].Content)
		for _, want := range []string{"useradd -m 'ops'", "'ALL=(ALL) NOPASSWD:ALL'", "'ssh-ed25519 AAAAops ops@template'"} {
			if !strings.Contains(script, want) {
				t.Errorf("users script lacks %q:\n%s", want, script)
			}
		}
	})

	t.Run("commands", func(t *testing.T) {
		// Scripts under /run are handed to an interpreter: /run is noexec.
		wantPre := []string{"/bin/bash " + EndpointFixScript, "/bin/bash " + JinjaScript, "/bin/bash " + UsersScript, "/etc/kube-vip-prepare.sh", "/bin/sh " + CommandsDir + "/pre-01.sh"}
		if !slices.Equal(c.PreCommands, wantPre) {
			t.Errorf("PreCommands = %v, want %v", c.PreCommands, wantPre)
		}
		if len(c.InstallCommands) != 2 ||
			c.InstallCommands[0] != "test -f "+JinjaDoneMarker+" && kubeadm init --config "+KubeadmConfigPath+" && "+sentinelCommand ||
			c.InstallCommands[1] != "echo done" {
			t.Errorf("InstallCommands = %q", c.InstallCommands)
		}
		if !slices.Equal(c.PostCommands, []string{"/bin/bash " + PostInitScript}) {
			t.Errorf("PostCommands = %v", c.PostCommands)
		}
		script := string(files[PostInitScript].Content)
		order := []string{
			"KUBECONFIG='/etc/kubernetes/admin.conf'",
			"https://10.0.0.10:6443",
			DefaultCNIManifestURL,
			DefaultCertManagerURL,
			"cert_manager_webhook_ready\n",
			DefaultOperatorURL,
			ManifestsDir + "/",
			"echo extra-step",
		}
		last := -1
		for _, s := range order {
			i := strings.LastIndex(script, s)
			if i < 0 {
				t.Errorf("post-init script lacks %q", s)
				continue
			}
			if i < last {
				t.Errorf("post-init script has %q out of order", s)
			}
			last = i
		}
	})

	t.Run("cloud-init", func(t *testing.T) {
		b, err := cloudinit.Build([]controlplane.Contribution{*c}, cloudinit.Options{Hostname: "demo-bootstrap"})
		if err != nil {
			t.Fatalf("cloudinit.Build: %v", err)
		}
		doc, err := cloudinit.Parse(b.UserData)
		if err != nil {
			t.Fatal(err)
		}
		if runcmd, _ := doc["runcmd"].([]any); len(runcmd) != 8 {
			t.Errorf("runcmd = %v", runcmd)
		}
	})
}

func TestUserDataRejects(t *testing.T) {
	p := testProvider()
	creds := prepare(t, p)
	for name, mutate := range map[string]func(string) string{
		"unknown jinja": func(s string) string {
			return strings.Replace(s, "ds.meta_data.local_hostname", "ds.meta_data.region", 1)
		},
		"jinja statement": func(s string) string {
			return strings.Replace(s, "- echo done", "- \"{% if true %}echo{% endif %}\"", 1)
		},
		"unsupported field": func(s string) string {
			return strings.Replace(s, "    preKubeadmCommands:", "    mounts:\n    - [a, b]\n    preKubeadmCommands:", 1)
		},
		"wrong kind": func(s string) string {
			return strings.Replace(s, "kind: KubeadmControlPlane", "kind: Other", 1)
		},
		"wrong version": func(s string) string {
			return strings.Replace(s, "controlplane.cluster.x-k8s.io/v1beta2", "controlplane.cluster.x-k8s.io/v1beta1", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := p.UserData(context.Background(), testSpec(), creds, controlPlaneObject(t, mutate(testKCP)), nil); err == nil {
				t.Error("expected an error")
			}
		})
	}
	t.Run("incomplete credentials", func(t *testing.T) {
		bad := *creds
		bad.Extra = map[string]cluster.KeyPair{"sa": creds.Extra["sa"], "proxy": creds.Extra["proxy"]}
		if _, err := p.UserData(context.Background(), testSpec(), &bad, controlPlaneObject(t, testKCP), nil); err == nil {
			t.Error("expected an error for a missing etcd CA")
		}
	})
}

func adopt(t *testing.T, p *Provider, cp *unstructured.Unstructured) (*clusterv1.Machine, *bootstrapv1.KubeadmConfig, *corev1.Secret) {
	t.Helper()
	state := &cluster.State{
		Spec:      *testSpec(),
		Endpoint:  cluster.Endpoint{Host: testVIP, Port: testPort},
		Bootstrap: &cluster.Node{ID: "6C3FA683-BEF9-4425-B413-EAA45A9D6191", Name: "demo-bootstrap"},
	}
	out, err := p.Adopt(context.Background(), state, cp)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	var (
		machine *clusterv1.Machine
		config  *bootstrapv1.KubeadmConfig
		data    *corev1.Secret
	)
	for _, doc := range splitDocs(string(out)) {
		var head struct{ Kind string }
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			t.Fatal(err)
		}
		switch head.Kind {
		case "Machine":
			machine = &clusterv1.Machine{}
			mustStrict(t, doc, machine)
		case "KubeadmConfig":
			config = &bootstrapv1.KubeadmConfig{}
			mustStrict(t, doc, config)
		case "Secret":
			data = &corev1.Secret{}
			mustStrict(t, doc, data)
		default:
			t.Errorf("unexpected kind %q", head.Kind)
		}
	}
	if machine == nil || config == nil || data == nil {
		t.Fatalf("missing documents: machine=%v config=%v secret=%v", machine != nil, config != nil, data != nil)
	}
	return machine, config, data
}

func TestAdopt(t *testing.T) {
	cp := controlPlaneObject(t, testKCP)
	kcp := typedKCP(t, cp)
	machine, config, data := adopt(t, testProvider(), cp)

	if machine.APIVersion != "cluster.x-k8s.io/v1beta2" || machine.Name != "demo-bootstrap" || machine.Namespace != "capi" {
		t.Errorf("machine header = %s %s/%s", machine.APIVersion, machine.Namespace, machine.Name)
	}
	if v, ok := machine.Labels[clusterv1.MachineControlPlaneLabel]; !ok || v != "" {
		t.Errorf("control-plane label = %q (present %v)", v, ok)
	}
	if machine.Labels[clusterv1.ClusterNameLabel] != "demo" || machine.Labels[clusterv1.MachineControlPlaneNameLabel] != "demo-control-plane" || machine.Labels["team"] != "infra" {
		t.Errorf("labels = %v", machine.Labels)
	}
	if len(machine.OwnerReferences) != 0 {
		t.Errorf("machine has owner references: %v", machine.OwnerReferences)
	}
	if !collections.AdoptableControlPlaneMachines("demo")(machine) {
		t.Error("machine is not adoptable by KubeadmControlPlane")
	}
	// KubeadmControlPlane compares spec.version as a string.
	if machine.Spec.ClusterName != "demo" || machine.Spec.Version != kcp.Spec.Version {
		t.Errorf("clusterName=%q version=%q", machine.Spec.ClusterName, machine.Spec.Version)
	}
	kcpVersion := semver.MustParse(strings.TrimPrefix(kcp.Spec.Version, "v"))
	mv, err := semver.ParseTolerant(machine.Spec.Version)
	if err != nil || !util.IsSupportedVersionSkew(kcpVersion, mv) {
		t.Errorf("machine version %q not within KCP skew (%v)", machine.Spec.Version, err)
	}
	wantConfigRef := clusterv1.ContractVersionedObjectReference{APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "KubeadmConfig", Name: "demo-bootstrap"}
	if machine.Spec.Bootstrap.ConfigRef != wantConfigRef {
		t.Errorf("configRef = %+v", machine.Spec.Bootstrap.ConfigRef)
	}
	if deref(machine.Spec.Bootstrap.DataSecretName) != "demo-bootstrap" {
		t.Errorf("dataSecretName = %v", machine.Spec.Bootstrap.DataSecretName)
	}
	wantInfraRef := clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "ProxmoxMachine", Name: "demo-bootstrap"}
	if machine.Spec.InfrastructureRef != wantInfraRef {
		t.Errorf("infrastructureRef = %+v", machine.Spec.InfrastructureRef)
	}
	if machine.Spec.ProviderID != "proxmox://6c3fa683-bef9-4425-b413-eaa45a9d6191" {
		t.Errorf("providerID = %q", machine.Spec.ProviderID)
	}

	if config.APIVersion != "bootstrap.cluster.x-k8s.io/v1beta2" || config.Name != "demo-bootstrap" || config.Namespace != "capi" {
		t.Errorf("config header = %s %s/%s", config.APIVersion, config.Namespace, config.Name)
	}
	if !reflect.DeepEqual(config.Labels, machine.Labels) || len(config.OwnerReferences) != 0 {
		t.Errorf("config labels = %v owners = %v", config.Labels, config.OwnerReferences)
	}

	// The spec is the KubeadmControlPlane's, byte for byte, apart from the two
	// changes KubeadmControlPlane itself makes for a first machine.
	want := kcp.Spec.KubeadmConfigSpec.DeepCopy()
	want.JoinConfiguration = bootstrapv1.JoinConfiguration{}
	want.ClusterConfiguration.FeatureGates = map[string]bool{controlPlaneKubeletLocalMode: true}
	if !reflect.DeepEqual(&config.Spec, want) {
		t.Errorf("KubeadmConfig spec differs from the KubeadmControlPlane's:\n got: %s\nwant: %s", toYAML(t, config.Spec), toYAML(t, want))
	}
	src := kcp.Spec.KubeadmConfigSpec
	if !reflect.DeepEqual(config.Spec.Files, src.Files) || !reflect.DeepEqual(config.Spec.Users, src.Users) ||
		!reflect.DeepEqual(config.Spec.PreKubeadmCommands, src.PreKubeadmCommands) ||
		!reflect.DeepEqual(config.Spec.PostKubeadmCommands, src.PostKubeadmCommands) ||
		!reflect.DeepEqual(config.Spec.InitConfiguration, src.InitConfiguration) ||
		!reflect.DeepEqual(config.Spec.ClusterConfiguration.APIServer, src.ClusterConfiguration.APIServer) {
		t.Error("files, users, commands, initConfiguration or apiServer differ from the KubeadmControlPlane")
	}

	if rollout, why := kcpRolloutNeeded(t, kcp, config); rollout {
		t.Errorf("KubeadmControlPlane would roll out the adopted machine: %s", why)
	}

	if data.Name != "demo-bootstrap" || data.Type != clusterv1.ClusterSecretType || data.Labels[clusterv1.ClusterNameLabel] != "demo" {
		t.Errorf("data secret = %s type %s labels %v", data.Name, data.Type, data.Labels)
	}
	if !strings.HasPrefix(string(data.Data["value"]), "#cloud-config") {
		t.Errorf("data secret value = %q", data.Data["value"])
	}
}

// TestAdoptMatcherDetectsDrift checks that the matcher replica below does
// report a rollout when the adopted spec diverges.
func TestAdoptMatcherDetectsDrift(t *testing.T) {
	cp := controlPlaneObject(t, testKCP)
	kcp := typedKCP(t, cp)
	_, config, _ := adopt(t, testProvider(), cp)
	config.Spec.Files[0].Content += "# drift\n"
	if rollout, _ := kcpRolloutNeeded(t, kcp, config); !rollout {
		t.Error("matcher did not detect a changed file")
	}

	// Init and join configurations that differ only work through the init
	// comparison, which needs joinConfiguration cleared on the adopted config.
	_, config, _ = adopt(t, testProvider(), cp)
	kcp.Spec.KubeadmConfigSpec.JoinConfiguration.NodeRegistration.KubeletExtraArgs = nil
	if rollout, why := kcpRolloutNeeded(t, kcp, config); rollout {
		t.Errorf("init comparison failed: %s", why)
	}
}

func TestAdoptVersionFallback(t *testing.T) {
	doc := strings.Replace(testKCP, "  version: v1.33.4\n", "", 1)
	cp := controlPlaneObject(t, doc)
	state := &cluster.State{Spec: *testSpec(), Endpoint: cluster.Endpoint{Host: testVIP, Port: testPort}}
	if _, err := New(Options{}).Adopt(context.Background(), state, cp); err == nil {
		t.Error("expected an error without any Kubernetes version")
	}
	out, err := New(Options{KubernetesVersion: "v1.32.1"}).Adopt(context.Background(), state, cp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "version: v1.32.1") {
		t.Error("Options.KubernetesVersion not used when spec.version is empty")
	}
	state.Spec.Provider = ""
	if _, err := New(Options{KubernetesVersion: "v1.32.1"}).Adopt(context.Background(), state, cp); err == nil {
		t.Error("expected an error without an infrastructure provider")
	}
}

func TestInfraMachineKind(t *testing.T) {
	for in, want := range map[string]string{
		"proxmox": "ProxmoxMachine",
		"Proxmox": "ProxmoxMachine",
		"docker":  "DockerMachine",
		"hyperv":  "HyperVMachine",
		"Hyperv":  "HyperVMachine",
		"foo":     "FooMachine",
		"":        "",
	} {
		if got := InfraMachineKind(in); got != want {
			t.Errorf("InfraMachineKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProviderID(t *testing.T) {
	for _, tc := range []struct{ provider, id, want string }{
		{"proxmox", "ABC-1", "proxmox://abc-1"},
		{"proxmox", "proxmox://x", "proxmox://x"},
		{"docker", "abc", ""},
		{"proxmox", "", ""},
	} {
		if got := providerID(tc.provider, tc.id); got != tc.want {
			t.Errorf("providerID(%q, %q) = %q, want %q", tc.provider, tc.id, got, tc.want)
		}
	}
}

func splitDocs(s string) []string {
	var out []string
	for _, d := range strings.Split(s, "\n---\n") {
		d = strings.TrimPrefix(strings.TrimSpace(d), "---")
		if strings.TrimSpace(d) != "" {
			out = append(out, d)
		}
	}
	return out
}

func mustStrict(t *testing.T, doc string, into any) {
	t.Helper()
	if err := yaml.UnmarshalStrict([]byte(doc), into); err != nil {
		t.Fatalf("document does not parse strictly: %v\n%s", err, doc)
	}
}

func toYAML(t *testing.T, v any) string {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func parseCert(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func TestUserDataNetworkFromCluster(t *testing.T) {
	p := testProvider()
	creds := prepare(t, p)
	cp := controlPlaneObject(t, testKCP)
	clusterObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.x-k8s.io/v1beta2", "kind": "Cluster",
		"metadata": map[string]any{"name": "demo"},
		"spec": map[string]any{"clusterNetwork": map[string]any{
			"pods":     map[string]any{"cidrBlocks": []any{"10.244.0.0/16", "fd00:10:244::/56"}},
			"services": map[string]any{"cidrBlocks": []any{"10.96.0.0/12"}},
		}},
	}}
	c, err := p.UserData(context.Background(), testSpec(), creds, cp, clusterObj)
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	var kubeadmYAML string
	for _, f := range c.Files {
		if f.Path == KubeadmConfigPath {
			kubeadmYAML = string(f.Content)
		}
	}
	for _, want := range []string{"podSubnet: 10.244.0.0/16,fd00:10:244::/56", "serviceSubnet: 10.96.0.0/12"} {
		if !strings.Contains(kubeadmYAML, want) {
			t.Errorf("kubeadm config lacks %q:\n%s", want, kubeadmYAML)
		}
	}

	// Without a Cluster object the provider's options stand in.
	c, err = p.UserData(context.Background(), testSpec(), creds, cp, nil)
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	for _, f := range c.Files {
		if f.Path == KubeadmConfigPath && !strings.Contains(string(f.Content), "podSubnet: "+p.opts.PodCIDR) {
			t.Errorf("option pod subnet missing:\n%s", f.Content)
		}
	}
}

// TestUserDataWithInstallScript checks that the install-at-boot patch flows
// through UserData as ordinary spec content: the script lands as a 0755 file
// outside /run and runs first among the spec's pre-kubeadm commands.
func TestUserDataWithInstallScript(t *testing.T) {
	b, err := generate.ParseBundle([]byte(testKCP))
	if err != nil {
		t.Fatal(err)
	}
	if err := generate.SetInstallKubernetes(b, generate.InstallKubernetesOptions{}); err != nil {
		t.Fatal(err)
	}
	p := testProvider()
	creds := prepare(t, p)
	c, err := p.UserData(context.Background(), testSpec(), creds, b.Objects[0], nil)
	if err != nil {
		t.Fatalf("UserData: %v", err)
	}
	var script *controlplane.File
	for i := range c.Files {
		if c.Files[i].Path == generate.InstallScriptPath {
			script = &c.Files[i]
		}
	}
	if script == nil {
		t.Fatalf("no %s among the files", generate.InstallScriptPath)
	}
	if script.Mode != "0755" || script.Owner != "root:root" || script.Append ||
		!strings.HasPrefix(string(script.Content), "#!/usr/bin/env bash\n") || !strings.Contains(string(script.Content), `WANT_VERSION="1.33.4"`) {
		t.Errorf("script file = mode %s owner %s append %v, content starts %q", script.Mode, script.Owner, script.Append, string(script.Content[:40]))
	}
	wantPre := []string{"/bin/bash " + EndpointFixScript, "/bin/bash " + JinjaScript, "/bin/bash " + UsersScript, generate.InstallScriptPath, "/etc/kube-vip-prepare.sh", "/bin/sh " + CommandsDir + "/pre-02.sh"}
	if !slices.Equal(c.PreCommands, wantPre) {
		t.Errorf("PreCommands = %v, want %v", c.PreCommands, wantPre)
	}
	// The adopted KubeadmConfig carries the same spec, so KubeadmControlPlane
	// sees the bootstrap machine as up to date.
	_, kc, _ := adopt(t, p, b.Objects[0])
	if kc.Spec.PreKubeadmCommands[0] != generate.InstallScriptPath {
		t.Errorf("adopted preKubeadmCommands = %v", kc.Spec.PreKubeadmCommands)
	}
}
