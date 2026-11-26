package docker

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"path"
	"strings"

	"sigs.k8s.io/yaml"
)

// cloudConfig is the subset of a cloud-config document this provider executes.
// Containers do not boot cloud-init, so klotho replays the write_files and
// runcmd directives by exec, the same shape CAPD turns bootstrap data into.
type cloudConfig struct {
	WriteFiles []cloudFile `json:"write_files"`
	RunCmd     []string    `json:"runcmd"`
}

type cloudFile struct {
	Path        string `json:"path"`
	Content     string `json:"content"`
	Encoding    string `json:"encoding"`
	Permissions string `json:"permissions"`
}

// runBootstrap replays userData inside the container: every write_files entry
// is created with its permissions, then every runcmd is run. The commands are
// the kubeadm init/join sequence klotho generated.
//
// NOTE: wired but not yet validated live. Containers run the commands through
// /bin/sh; a kindest/node image has kubeadm, kubelet and containerd available.
func (p *Provider) runBootstrap(ctx context.Context, container string, userData []byte) error {
	var cc cloudConfig
	if err := yaml.Unmarshal(userData, &cc); err != nil {
		return fmt.Errorf("parsing bootstrap cloud-config: %w", err)
	}
	for _, f := range cc.WriteFiles {
		if f.Path == "" {
			continue
		}
		body, err := decodeContent(f.Content, f.Encoding)
		if err != nil {
			return fmt.Errorf("decoding %s: %w", f.Path, err)
		}
		if _, err := p.rt.Exec(ctx, container, []string{"mkdir", "-p", path.Dir(f.Path)}, nil); err != nil {
			return fmt.Errorf("mkdir for %s: %w", f.Path, err)
		}
		if _, err := p.rt.Exec(ctx, container, []string{"cp", "/dev/stdin", f.Path}, body); err != nil {
			return fmt.Errorf("writing %s: %w", f.Path, err)
		}
		if f.Permissions != "" {
			if _, err := p.rt.Exec(ctx, container, []string{"chmod", f.Permissions, f.Path}, nil); err != nil {
				return fmt.Errorf("chmod %s: %w", f.Path, err)
			}
		}
	}
	for _, cmd := range cc.RunCmd {
		if strings.TrimSpace(cmd) == "" {
			continue
		}
		if _, err := p.rt.Exec(ctx, container, []string{"/bin/sh", "-c", cmd}, nil); err != nil {
			return fmt.Errorf("runcmd %q: %w", cmd, err)
		}
	}
	return nil
}

// decodeContent decodes a write_files content body by its encoding
// ("", "b64"/"base64", "gz+base64"/"gzip+base64").
func decodeContent(content, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "text/plain":
		return []byte(content), nil
	case "b64", "base64":
		return base64.StdEncoding.DecodeString(content)
	case "gz+base64", "gzip+base64", "gz", "gzip":
		raw, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, err
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		return io.ReadAll(zr)
	default:
		return nil, fmt.Errorf("unsupported encoding %q", encoding)
	}
}

// configureLoadBalancer points the cluster's haproxy load balancer at the
// bootstrap container and reloads it, so the control plane endpoint returned by
// Prepare reaches the new API server.
//
// NOTE: wired but not yet validated live. CAPD's load balancer controller does
// the equivalent (UpdateConfiguration writes haproxy.cfg and SIGHUPs); klotho
// does it once for the bootstrap node, and CAPD takes over the config after it
// adopts the load balancer.
func (p *Provider) configureLoadBalancer(ctx context.Context, clusterName, container string) error {
	lb := LBName(clusterName)
	backendIP, err := p.containerIP(ctx, container)
	if err != nil {
		return err
	}
	cfg := haproxyConfig(backendIP, DefaultPort)
	if _, err := p.rt.Exec(ctx, lb, []string{"cp", "/dev/stdin", "/usr/local/etc/haproxy/haproxy.cfg"}, []byte(cfg)); err != nil {
		return fmt.Errorf("writing haproxy.cfg: %w", err)
	}
	// haproxy runs as PID 1; a SIGHUP triggers a config reload.
	if _, err := p.rt.Exec(ctx, lb, []string{"kill", "-s", "HUP", "1"}, nil); err != nil {
		return fmt.Errorf("reloading haproxy: %w", err)
	}
	return nil
}

// haproxyConfig renders a minimal haproxy.cfg forwarding the control plane
// port to a single backend.
func haproxyConfig(backendIP string, port int) string {
	return fmt.Sprintf(`defaults
  mode tcp
  timeout connect 5s
  timeout client 30s
  timeout server 30s

frontend control-plane
  bind *:%d
  default_backend kube-apiservers

backend kube-apiservers
  option tcp-check
  server bootstrap %s:%d check
`, port, backendIP, port)
}
