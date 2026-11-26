package credential

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"slices"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// testCA returns an RSA CA encoded the way Cluster API encodes it (PKCS#1).
func testCA(t *testing.T) cluster.KeyPair {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kubernetes"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return cluster.KeyPair{
		Cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Key:  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	}
}

func TestMint(t *testing.T) {
	ca := testCA(t)
	start := time.Now()
	certPEM, keyPEM, err := Mint(ca, "demo", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(certPEM, ca.Cert); err != nil {
		t.Fatalf("cert does not verify against CA: %v", err)
	}
	cert, err := ParseCertificate(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != CommonName {
		t.Errorf("CN = %q", cert.Subject.CommonName)
	}
	if !slices.Equal(cert.Subject.Organization, []string{Group}) {
		t.Errorf("O = %v", cert.Subject.Organization)
	}
	if d := cert.NotAfter.Sub(start); d < 29*time.Minute || d > 31*time.Minute {
		t.Errorf("NotAfter is %s after start, want ~30m", d)
	}
	if d := start.Sub(cert.NotBefore); d < 4*time.Minute || d > 6*time.Minute {
		t.Errorf("NotBefore is %s before start, want ~5m backdate", d)
	}
	if !slices.Contains(cert.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		t.Errorf("missing client auth usage")
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		t.Fatal("no key PEM")
	}
	if _, err := x509.ParseECPrivateKey(block.Bytes); err != nil {
		t.Fatalf("key is not ECDSA: %v", err)
	}
}

func TestMintDefaultTTL(t *testing.T) {
	certPEM, _, err := Mint(testCA(t), "demo", 0)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := ParseCertificate(certPEM)
	if d := time.Until(cert.NotAfter); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("default TTL gives %s", d)
	}
}

func TestMintErrors(t *testing.T) {
	ca := testCA(t)
	if _, _, err := Mint(cluster.KeyPair{Cert: ca.Cert}, "demo", 0); err == nil {
		t.Error("want error without CA key")
	}
	if _, _, err := Mint(cluster.KeyPair{Key: ca.Key}, "demo", 0); err == nil {
		t.Error("want error without CA cert")
	}
}

func TestVerifyOtherCA(t *testing.T) {
	certPEM, _, err := Mint(testCA(t), "demo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(certPEM, testCA(t).Cert); err == nil {
		t.Fatal("cert verified against an unrelated CA")
	}
}

func TestExecCredential(t *testing.T) {
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	b, err := ExecCredential([]byte("CERT"), []byte("KEY"), exp)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["apiVersion"] != "client.authentication.k8s.io/v1" || m["kind"] != "ExecCredential" {
		t.Fatalf("header: %v", m)
	}
	st := m["status"].(map[string]any)
	if st["clientCertificateData"] != "CERT" || st["clientKeyData"] != "KEY" || st["expirationTimestamp"] != "2030-01-02T03:04:05Z" {
		t.Fatalf("status: %v", st)
	}
	c, k, e, err := ParseExecCredential(b)
	if err != nil || string(c) != "CERT" || string(k) != "KEY" || !e.Equal(exp) {
		t.Fatalf("round trip: %q %q %v %v", c, k, e, err)
	}
}

func TestKubeconfig(t *testing.T) {
	ca := testCA(t)
	b, err := Kubeconfig("demo", cluster.Endpoint{Host: "10.0.0.5", Port: 6443}, ca.Cert, "/usr/local/bin/klotho", []string{"--profile", "lab"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(b)
	if err != nil {
		t.Fatal(err)
	}
	ctx := cfg.Contexts[cfg.CurrentContext]
	if ctx == nil {
		t.Fatal("no current context")
	}
	cl := cfg.Clusters[ctx.Cluster]
	if cl.Server != "https://10.0.0.5:6443" || string(cl.CertificateAuthorityData) != string(ca.Cert) {
		t.Fatalf("cluster: %+v", cl)
	}
	ex := cfg.AuthInfos[ctx.AuthInfo].Exec
	if ex == nil {
		t.Fatal("no exec block")
	}
	if ex.Command != "/usr/local/bin/klotho" || !slices.Equal(ex.Args, []string{"credential", "demo", "--profile", "lab"}) {
		t.Fatalf("exec: %s %v", ex.Command, ex.Args)
	}
	if ex.APIVersion != ExecAPIVersion || ex.InteractiveMode != clientcmdapi.NeverExecInteractiveMode || ex.ProvideClusterInfo {
		t.Fatalf("exec settings: %+v", ex)
	}
	if len(cfg.AuthInfos[ctx.AuthInfo].ClientCertificateData) != 0 {
		t.Fatal("kubeconfig must not embed a client certificate")
	}
}
