package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/credential"
)

// Credential cache policy: a cached certificate is reused while more than
// credentialMinRemaining of its lifetime is left.
const (
	credentialTTL          = time.Hour
	credentialMinRemaining = 5 * time.Minute
)

// newCredentialCommand is the kubectl exec credential plugin target. It is
// invoked by kubeconfigs written by the shell command, never by people.
func newCredentialCommand() *cli.Command {
	return &cli.Command{
		Name:      "credential",
		Usage:     "Emit a short-lived client credential for a cluster (exec plugin)",
		ArgsUsage: "<name>",
		Hidden:    true,
		Action: func(ctx context.Context, c *cli.Command) error {
			name, err := clusterNameArg(c)
			if err != nil {
				return err
			}
			a, err := newApp(c)
			if err != nil {
				return err
			}
			st, err := klotho.Load(ctx, a.deps(), name)
			if err != nil {
				return err
			}
			out, err := execCredential(a.cfg.CredentialsDir(), st, time.Now())
			if err != nil {
				return err
			}
			_, err = stdout(c).Write(out)
			return err
		},
	}
}

// execCredential returns the cached ExecCredential for the cluster when it
// still verifies against the state's CA and has enough lifetime left, and
// otherwise mints, caches and returns a new one.
func execCredential(dir string, st *cluster.State, now time.Time) ([]byte, error) {
	name := st.Spec.Name
	if st.Reserved {
		return nil, fmt.Errorf("cluster %s is only reserved; it has no credentials yet", name)
	}
	if len(st.CA.Cert) == 0 || len(st.CA.Key) == 0 {
		return nil, fmt.Errorf("cluster %s: state has no CA", name)
	}
	path := filepath.Join(dir, name+".json")
	if b, err := os.ReadFile(path); err == nil {
		cert, _, expiry, err := credential.ParseExecCredential(b)
		if err == nil && expiry.Sub(now) > credentialMinRemaining && credential.Verify(cert, st.CA.Cert) == nil {
			return b, nil
		}
	}
	cert, key, err := credential.Mint(st.CA, name, credentialTTL)
	if err != nil {
		return nil, err
	}
	parsed, err := credential.ParseCertificate(cert)
	if err != nil {
		return nil, err
	}
	out, err := credential.ExecCredential(cert, key, parsed.NotAfter)
	if err != nil {
		return nil, err
	}
	if err := writePrivate(dir, name+".json", out); err != nil {
		// The cache is an optimization; a credential is still returned.
		fmt.Fprintf(os.Stderr, "klotho: caching credential: %v\n", err)
	}
	return out, nil
}

// writePrivate atomically writes a 0600 file in a 0700 directory.
func writePrivate(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
