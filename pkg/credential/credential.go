// Package credential mints short-lived client credentials for a cluster from
// the CA persisted in its state, and renders the kubectl exec-plugin
// artifacts (ExecCredential and kubeconfig) that deliver them.
//
// No long-lived credential is ever written to a kubeconfig: the kubeconfig
// runs "klotho credential <name>", which mints a certificate on demand.
package credential

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

const (
	// CommonName is the user name the API server sees for minted
	// certificates.
	CommonName = "klotho-admin"
	// Group is the organization of minted certificates. system:masters is
	// bound to cluster-admin and bypasses RBAC entirely. That is intentional
	// for now: klotho's user is the cluster's owner, and the certificate's
	// short lifetime is what bounds the exposure. A narrower group bound by
	// an RBAC object the cluster carries is the expected replacement.
	Group = "system:masters"

	// DefaultTTL is the certificate lifetime when Mint is given zero.
	DefaultTTL = time.Hour
	// Backdate is subtracted from NotBefore to tolerate clock skew between
	// this machine and the API server.
	Backdate = 5 * time.Minute

	// ExecAPIVersion is the client authentication API version used by the
	// ExecCredential and the kubeconfig exec block.
	ExecAPIVersion = "client.authentication.k8s.io/v1"
)

// now is replaced by tests.
var now = time.Now

// Mint issues a client certificate for clusterName signed by ca: CN
// CommonName, O Group, an ECDSA P-256 key, valid from now-Backdate until
// now+ttl (DefaultTTL when ttl is zero).
func Mint(ca cluster.KeyPair, clusterName string, ttl time.Duration) (clientCertPEM, clientKeyPEM []byte, err error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	caCert, err := ParseCertificate(ca.Cert)
	if err != nil {
		return nil, nil, fmt.Errorf("credential: cluster %s: CA certificate: %w", clusterName, err)
	}
	if len(ca.Key) == 0 {
		return nil, nil, fmt.Errorf("credential: cluster %s: state has no CA key", clusterName)
	}
	caKey, err := parsePrivateKey(ca.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("credential: cluster %s: CA key: %w", clusterName, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("credential: generating key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, fmt.Errorf("credential: generating serial: %w", err)
	}
	t := now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: CommonName, Organization: []string{Group}},
		NotBefore:             t.Add(-Backdate).UTC(),
		NotAfter:              t.Add(ttl).UTC(),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("credential: signing certificate for cluster %s: %w", clusterName, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("credential: encoding key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// ParseCertificate decodes the first PEM certificate in b.
func ParseCertificate(b []byte) (*x509.Certificate, error) {
	rest := b
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no PEM certificate found")
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
}

// Verify reports whether certPEM is a client certificate signed by caPEM and
// valid at the current time.
func Verify(certPEM, caPEM []byte) error {
	cert, err := ParseCertificate(certPEM)
	if err != nil {
		return err
	}
	ca, err := ParseCertificate(caPEM)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	_, err = cert.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now(),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return err
}

func parsePrivateKey(b []byte) (any, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("no PEM key found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		switch k.(type) {
		case *rsa.PrivateKey, *ecdsa.PrivateKey:
			return k, nil
		}
		return nil, fmt.Errorf("unsupported key type %T", k)
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("unsupported private key encoding %q", block.Type)
}

// execCredential is client.authentication.k8s.io/v1 ExecCredential, kept
// local so the JSON shape is explicit.
type execCredential struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Spec       struct{}             `json:"spec"`
	Status     execCredentialStatus `json:"status"`
}

type execCredentialStatus struct {
	ExpirationTimestamp   string `json:"expirationTimestamp,omitempty"`
	ClientCertificateData string `json:"clientCertificateData"`
	ClientKeyData         string `json:"clientKeyData"`
}

// ExecCredential renders the ExecCredential a kubectl exec plugin prints.
func ExecCredential(certPEM, keyPEM []byte, expiry time.Time) ([]byte, error) {
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, errors.New("credential: certificate and key are required")
	}
	ec := execCredential{APIVersion: ExecAPIVersion, Kind: "ExecCredential"}
	ec.Status.ClientCertificateData = string(certPEM)
	ec.Status.ClientKeyData = string(keyPEM)
	if !expiry.IsZero() {
		ec.Status.ExpirationTimestamp = expiry.UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(ec)
	if err != nil {
		return nil, fmt.Errorf("credential: encoding ExecCredential: %w", err)
	}
	return append(b, '\n'), nil
}

// ParseExecCredential is the inverse of ExecCredential.
func ParseExecCredential(b []byte) (certPEM, keyPEM []byte, expiry time.Time, err error) {
	var ec execCredential
	if err := json.Unmarshal(b, &ec); err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("credential: decoding ExecCredential: %w", err)
	}
	if ec.APIVersion != ExecAPIVersion || ec.Kind != "ExecCredential" {
		return nil, nil, time.Time{}, fmt.Errorf("credential: not a %s ExecCredential", ExecAPIVersion)
	}
	if ec.Status.ExpirationTimestamp != "" {
		if expiry, err = time.Parse(time.RFC3339, ec.Status.ExpirationTimestamp); err != nil {
			return nil, nil, time.Time{}, fmt.Errorf("credential: expirationTimestamp: %w", err)
		}
	}
	return []byte(ec.Status.ClientCertificateData), []byte(ec.Status.ClientKeyData), expiry, nil
}

// WithServer returns a copy of kubeconfig with every cluster's server URL set
// to ep. It lets klotho reuse an admin kubeconfig (client certificate and CA
// unchanged) while dialing a host-reachable endpoint different from the one it
// was minted for, for clusters whose controlPlaneEndpoint the host cannot route
// (see cluster.State.ClientEndpoint).
func WithServer(kubeconfig []byte, ep cluster.Endpoint) ([]byte, error) {
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("credential: parsing kubeconfig: %w", err)
	}
	server := ServerURL(ep)
	for _, c := range cfg.Clusters {
		c.Server = server
	}
	out, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("credential: serializing kubeconfig: %w", err)
	}
	return out, nil
}

// ServerURL returns https://host:port for an endpoint (port 6443 when unset).
func ServerURL(ep cluster.Endpoint) string {
	port := ep.Port
	if port == 0 {
		port = 6443
	}
	return "https://" + net.JoinHostPort(ep.Host, strconv.Itoa(port))
}

// Kubeconfig renders a kubeconfig for clusterName whose user runs
// "klothoPath credential <clusterName> [extraArgs...]" as an exec plugin.
// The cluster CA is inlined; no client credential is.
func Kubeconfig(clusterName string, endpoint cluster.Endpoint, caCert []byte, klothoPath string, extraArgs []string) ([]byte, error) {
	if clusterName == "" {
		return nil, errors.New("credential: cluster name is required")
	}
	if endpoint.Host == "" {
		return nil, errors.New("credential: endpoint host is required")
	}
	if len(caCert) == 0 {
		return nil, errors.New("credential: cluster CA certificate is required")
	}
	if klothoPath == "" {
		return nil, errors.New("credential: klotho path is required")
	}
	user := CommonName + "@" + clusterName
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   ServerURL(endpoint),
		CertificateAuthorityData: caCert,
	}
	cfg.AuthInfos[user] = &clientcmdapi.AuthInfo{
		Exec: &clientcmdapi.ExecConfig{
			APIVersion:         ExecAPIVersion,
			Command:            klothoPath,
			Args:               append([]string{"credential", clusterName}, extraArgs...),
			InteractiveMode:    clientcmdapi.NeverExecInteractiveMode,
			ProvideClusterInfo: false,
		},
	}
	cfg.Contexts[clusterName] = &clientcmdapi.Context{Cluster: clusterName, AuthInfo: user}
	cfg.CurrentContext = clusterName
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("credential: writing kubeconfig: %w", err)
	}
	return b, nil
}
