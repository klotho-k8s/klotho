package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// CLIRuntime drives docker or podman through its CLI. It shells out rather
// than using a client library so klotho carries no engine SDK and works
// against whichever of docker/podman is on PATH.
//
// NOTE: the control plane run recipe below mirrors kind's/CAPD's node
// container setup (privileged, cgroupns, tmpfs for /tmp and /run, /sbin/init
// as entrypoint). It is wired but has NOT been validated against a live
// daemon yet; see the package tests, which use a fake Runtime.
type CLIRuntime struct {
	bin string
}

// NewCLIRuntime returns a CLIRuntime using the given binary, or "docker" when
// empty.
func NewCLIRuntime(bin string) *CLIRuntime {
	if bin == "" {
		bin = "docker"
	}
	return &CLIRuntime{bin: bin}
}

func (r *CLIRuntime) run(ctx context.Context, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.bin, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", r.bin, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// EnsureNetwork implements Runtime.
func (r *CLIRuntime) EnsureNetwork(ctx context.Context, name string) error {
	if _, err := r.run(ctx, nil, "network", "inspect", name); err == nil {
		return nil
	}
	_, err := r.run(ctx, nil, "network", "create", name)
	return err
}

// RunNode implements Runtime.
func (r *CLIRuntime) RunNode(ctx context.Context, opts RunOpts) (string, error) {
	args := []string{"run", "-d", "--name", opts.Name, "--hostname", opts.Name}
	labels := opts.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range labels {
		args = append(args, "--label", k+"="+v)
	}
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
	}
	if opts.PublishAPIPort {
		args = append(args, "--publish", fmt.Sprintf("%d", DefaultPort))
	}
	switch opts.Role {
	case RoleControlPlane, RoleWorker:
		// The kindest/node recipe: an init container that can run kubelet and
		// containerd. Matches kind's node create options. Validated on podman.
		args = append(args,
			"--privileged",
			"--security-opt", "seccomp=unconfined",
			"--security-opt", "apparmor=unconfined",
			"--tmpfs", "/tmp",
			"--tmpfs", "/run",
			"--volume", "/var",
			"--restart", "on-failure:1",
			"--cgroupns", "private",
			// ip_forward is network-namespaced and defaults to 0 in a fresh
			// container netns, which fails kubeadm's preflight
			// (ERROR FileContent--proc-sys-net-ipv4-ip_forward). kind enables it
			// the same way. Docker Desktop happens to have it on already;
			// podman does not, so set it explicitly for both.
			"--sysctl", "net.ipv4.ip_forward=1",
		)
		if opts.SocketMount != "" {
			args = append(args, "--volume", opts.SocketMount+":"+DockerSocketPath)
		}
	}
	args = append(args, opts.Image)
	out, err := r.run(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Exec implements Runtime.
func (r *CLIRuntime) Exec(ctx context.Context, name string, cmd []string, stdin []byte) (string, error) {
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	args = append(args, name)
	args = append(args, cmd...)
	return r.run(ctx, stdin, args...)
}

// psEntry is the subset of `docker ps --format json` this provider reads.
// docker and podman disagree on two fields: docker renders Names as a string
// and Labels as a comma-separated "k=v" string, while podman renders Names as
// a string array and Labels as an object. Both are decoded as raw JSON and
// normalized by name and parseLabels.
type psEntry struct {
	ID     string          `json:"ID"`
	Names  json.RawMessage `json:"Names"`
	Image  string          `json:"Image"`
	State  string          `json:"State"`
	Labels json.RawMessage `json:"Labels"`
}

// List implements Runtime.
func (r *CLIRuntime) List(ctx context.Context, selector map[string]string) ([]Container, error) {
	args := []string{"ps", "--all", "--no-trunc", "--format", "{{json .}}"}
	for k, v := range selector {
		args = append(args, "--filter", "label="+k+"="+v)
	}
	out, err := r.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var cs []Container
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e psEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("parsing container list: %w", err)
		}
		c := Container{
			ID:      e.ID,
			Name:    firstName(e.Names),
			Image:   e.Image,
			Running: strings.EqualFold(e.State, "running"),
			Labels:  parseLabels(e.Labels),
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// Inspect implements Runtime.
func (r *CLIRuntime) Inspect(ctx context.Context, name string) (Container, bool, error) {
	out, err := r.run(ctx, nil, "inspect", name)
	if err != nil {
		// `inspect` fails when the container is absent; treat as not found.
		return Container{}, false, nil
	}
	var raw []struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil || len(raw) == 0 {
		return Container{}, false, fmt.Errorf("parsing inspect of %s: %w", name, err)
	}
	c := Container{
		ID:      raw[0].ID,
		Name:    strings.TrimPrefix(raw[0].Name, "/"),
		Image:   raw[0].Config.Image,
		Running: raw[0].State.Running,
		Labels:  raw[0].Config.Labels,
	}
	for _, n := range raw[0].NetworkSettings.Networks {
		if n.IPAddress != "" {
			c.IP = n.IPAddress
			break
		}
	}
	return c, true, nil
}

// Remove implements Runtime.
func (r *CLIRuntime) Remove(ctx context.Context, name string) error {
	_, err := r.run(ctx, nil, "rm", "--force", "--volumes", name)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

// RemoveNetwork implements Runtime. A missing network, or one still in use by
// another cluster's containers, is tolerated: the network is shared, so a
// failure to remove it is not a failure of the teardown.
func (r *CLIRuntime) RemoveNetwork(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	switch name {
	case "podman", "bridge", "host", "none":
		// Built-in networks are never removed.
		return nil
	}
	_, err := r.run(ctx, nil, "network", "rm", name)
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "No such network") || strings.Contains(msg, "not found") ||
		strings.Contains(msg, "has active endpoints") || strings.Contains(msg, "in use") {
		return nil
	}
	return err
}

// PublishedPort implements Runtime. It reads the host binding of a container's
// published port with `<bin> port <name> <containerPort>/tcp`, which both
// docker and podman render as "host:port" lines (for example "0.0.0.0:35623").
// A wildcard host (0.0.0.0 or ::) is normalized to 127.0.0.1.
func (r *CLIRuntime) PublishedPort(ctx context.Context, name string, containerPort int) (string, int, error) {
	out, err := r.run(ctx, nil, "port", name, fmt.Sprintf("%d/tcp", containerPort))
	if err != nil {
		return "", 0, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		host, port, err := net.SplitHostPort(line)
		if err != nil {
			continue
		}
		p, err := strconv.Atoi(port)
		if err != nil || p == 0 {
			continue
		}
		if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
			host = "127.0.0.1"
		}
		return host, p, nil
	}
	return "", 0, fmt.Errorf("%s port %s %d: no published host port in %q", r.bin, name, containerPort, strings.TrimSpace(out))
}

// RunCleanup implements Runtime. It launches the cleanup container detached and
// self-removing (--rm): the runtime removes it when its sweep script exits,
// which is the reliable way for the container to disappear (a container that
// force-removes itself over the socket is SIGKILLed mid-removal on podman and
// can leak). The sweep is passed as a single `sh -c` argument and reads its
// parameters from the environment.
func (r *CLIRuntime) RunCleanup(ctx context.Context, opts CleanupOpts) (string, error) {
	if opts.Socket == "" {
		return "", fmt.Errorf("docker provider: deletion cleanup needs the host runtime socket; set docker.socket or use `klotho delete --watch`")
	}
	args := []string{
		"run", "--detach", "--rm",
		"--name", opts.Name,
		"--label", CleanupLabelKey + "=" + opts.Cluster,
		"--volume", opts.Socket + ":" + DockerSocketPath,
		"--env", "DOCKER_HOST=unix://" + DockerSocketPath,
		"--env", "CLUSTER=" + opts.Cluster,
		"--env", "NETWORK=" + opts.Network,
		"--env", fmt.Sprintf("TIMEOUT=%d", opts.TimeoutSeconds),
		opts.Image,
		"sh", "-c", opts.Script,
	}
	out, err := r.run(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// firstName returns the first container name from the Names field, which
// docker renders as a (possibly comma-separated) string and podman as a string
// array.
func firstName(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '[' {
		var names []string
		if err := json.Unmarshal(raw, &names); err == nil && len(names) > 0 {
			return names[0]
		}
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.Split(s, ",")[0]
}

// parseLabels parses the Labels field, which docker ps emits as a
// comma-separated "k=v,k2=v2" string and podman ps as a JSON object.
func parseLabels(raw json.RawMessage) map[string]string {
	m := map[string]string{}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return m
	}
	if raw[0] == '{' {
		_ = json.Unmarshal(raw, &m)
		return m
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return m
	}
	for _, kv := range strings.Split(s, ",") {
		if kv == "" {
			continue
		}
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}
