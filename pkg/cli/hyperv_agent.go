package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
	"github.com/klotho-k8s/klotho/pkg/config"
)

// The Hyper-V host agent (hyperv-agent) is a gRPC daemon on the Windows host
// that the in-cluster Cluster API Hyper-V controller drives over the network.
// klotho boots node zero itself over local PowerShell, but the controller
// cannot, so "klotho create --provider hyperv" makes sure the agent is present
// and feeds its endpoint, CA and token into the operator's configSecret.
const (
	// agentServiceName is the Windows service the agent registers.
	agentServiceName = "hyperv-agent"
	// DefaultAgentEndpoint is the agent's host:port when none is configured.
	// The agent binds the host side of the Default Switch NAT network, which
	// is what an in-cluster pod reaches the host on.
	DefaultAgentEndpoint = "172.30.32.1:7443"
	// DefaultAgentDataDir is the locked-down directory the agent stores its
	// cert, key and token in.
	DefaultAgentDataDir = `C:\ProgramData\hyperv-agent`
)

// DefaultAgentCAFile is the agent's self-signed CA certificate, written next
// to the service's data directory at install time.
var DefaultAgentCAFile = filepath.Join(DefaultAgentDataDir, "agent.crt")

// agentProbe abstracts the two host checks detection needs so tests can fake
// the Windows service control manager and a live daemon.
type agentProbe struct {
	// serviceInstalled reports whether the named Windows service exists.
	serviceInstalled func(ctx context.Context, name string) (bool, error)
	// endpointReachable reports whether a TCP connection to endpoint succeeds.
	endpointReachable func(ctx context.Context, endpoint string) bool
}

// defaultAgentProbe probes the real host: sc.exe for the service and a short
// TCP dial for the endpoint.
func defaultAgentProbe() agentProbe {
	return agentProbe{
		serviceInstalled: scServiceInstalled,
		endpointReachable: func(ctx context.Context, endpoint string) bool {
			d := net.Dialer{Timeout: 2 * time.Second}
			conn, err := d.DialContext(ctx, "tcp", endpoint)
			if err != nil {
				return false
			}
			_ = conn.Close()
			return true
		},
	}
}

// scServiceInstalled queries the service control manager for name.
func scServiceInstalled(ctx context.Context, name string) (bool, error) {
	if runtime.GOOS != "windows" {
		return false, nil
	}
	out, err := exec.CommandContext(ctx, "sc.exe", "query", name).CombinedOutput()
	s := string(out)
	switch {
	case err == nil:
		return true, nil
	// 1060: the specified service does not exist as an installed service.
	case strings.Contains(s, "1060") || strings.Contains(strings.ToLower(s), "does not exist"):
		return false, nil
	default:
		// Unknown sc.exe failure: treat as "not installed" but surface it so a
		// caller can decide. A missing service and a broken SCM both mean the
		// agent is not usable.
		return false, nil
	}
}

// agentResolution is what detection resolves for the configSecret.
type agentResolution struct {
	Endpoint  string
	CA        string
	Token     string
	Installed bool
	Reachable bool
}

// resolveHyperVAgent gathers the endpoint, CA and token for the in-cluster
// controller from configuration, the environment and host defaults, and probes
// whether the agent is installed and reachable. It never installs anything; it
// is the pure core the create hook and tests share. readFile is injected so
// tests need not touch the real C:\ProgramData path.
func resolveHyperVAgent(ctx context.Context, h config.Hyperv, probe agentProbe, readFile func(string) ([]byte, error)) (agentResolution, error) {
	res := agentResolution{Endpoint: h.ResolvedAgentEndpoint()}
	if res.Endpoint == "" {
		res.Endpoint = DefaultAgentEndpoint
	}

	ca, err := h.ResolvedAgentCA()
	if err != nil {
		return res, err
	}
	if ca == "" {
		// Fall back to the agent's default install location.
		if b, err := readFile(DefaultAgentCAFile); err == nil {
			ca = string(b)
		}
	}
	res.CA = ca

	tok, err := h.ResolvedAgentToken()
	if err != nil {
		return res, err
	}
	res.Token = tok

	if probe.serviceInstalled != nil {
		if installed, err := probe.serviceInstalled(ctx, agentServiceName); err == nil {
			res.Installed = installed
		}
	}
	if probe.endpointReachable != nil {
		res.Reachable = probe.endpointReachable(ctx, res.Endpoint)
	}
	return res, nil
}

// ensureHyperVAgent makes the Hyper-V agent available for a create on Windows
// and writes the resolved endpoint, CA and token back onto the configuration so
// providerSecretData emits them. On a non-Windows host it is a no-op (nothing
// here can reach the host's Hyper-V) and the configured values pass through.
func (a *app) ensureHyperVAgent(ctx context.Context, c *cli.Command) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	res, err := resolveHyperVAgent(ctx, a.cfg.Hyperv, defaultAgentProbe(), os.ReadFile)
	if err != nil {
		return err
	}

	if !res.Installed && !res.Reachable {
		installed, err := a.offerAgentInstall(ctx, c, res.Endpoint)
		if err != nil {
			return err
		}
		if !installed {
			return fmt.Errorf("hyperv agent not available: install it (klotho hyperv-agent install) or set hyperv.agentEndpoint/agentCa/agentToken")
		}
		// Re-resolve now that the service is installed.
		if res, err = resolveHyperVAgent(ctx, a.cfg.Hyperv, defaultAgentProbe(), os.ReadFile); err != nil {
			return err
		}
	}

	if res.CA == "" {
		return fmt.Errorf("hyperv agent CA not found: set hyperv.agentCa or hyperv.agentCaFile (default %s)", DefaultAgentCAFile)
	}
	if res.Token == "" {
		return fmt.Errorf("hyperv agent token not found: set hyperv.agentToken or hyperv.agentTokenFile (the token the service was installed with)")
	}

	a.cfg.Hyperv.AgentEndpoint = res.Endpoint
	a.cfg.Hyperv.AgentCA = res.CA
	a.cfg.Hyperv.AgentToken = res.Token
	_, _ = fmt.Fprintf(stderr(c), "hyperv: using agent at %s (service installed=%v reachable=%v)\n", res.Endpoint, res.Installed, res.Reachable)
	return nil
}

// offerAgentInstall prompts to install the agent (honouring --yes and
// --no-install-agent) and installs it when allowed. It returns whether the
// agent ends up installed.
func (a *app) offerAgentInstall(ctx context.Context, c *cli.Command, endpoint string) (bool, error) {
	if c.Bool("no-install-agent") {
		return false, nil
	}
	assume := c.Bool("yes")
	if !assume {
		if !interactive(c) {
			return false, fmt.Errorf("hyperv agent not found and input is not interactive: pass --yes to install it, or --no-install-agent to skip")
		}
		_, _ = fmt.Fprint(stderr(c), "Hyper-V agent not found. Install it now? [Y/n] ")
		r := bufio.NewReader(stdin(c))
		line, _ := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
		default:
			return false, nil
		}
	}
	if err := installHyperVAgent(ctx, agentInstallOptions{
		Endpoint: endpoint,
		Binary:   a.cfg.Hyperv.AgentBinary,
		Out:      stderr(c),
	}); err != nil {
		return false, err
	}
	return true, nil
}

// interactive reports whether stdin is a terminal the prompt can read from.
func interactive(c *cli.Command) bool {
	f, ok := stdin(c).(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// agentInstallOptions configures an install.
type agentInstallOptions struct {
	// Endpoint is the host:port the service binds. Empty means the agent's
	// own default.
	Endpoint string
	// Token is the bearer token; empty mints a fresh random one.
	Token string
	// Binary is a path to the agent exe. Empty requires one to already be at
	// the default install path (downloading a release asset is a TODO tracked
	// for the provider's first tagged release).
	Binary string
	// DataDir overrides the service data directory.
	DataDir string
	// Out receives progress lines.
	Out interface{ Write([]byte) (int, error) }
}

// mintToken returns a fresh random bearer token.
func mintToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// installHyperVAgent registers and starts the Windows service by shelling to
// the agent executable's own "install" subcommand, which owns the sc.exe and
// DACL details. It elevates through a UAC "runas" when the current process is
// not already an administrator.
func installHyperVAgent(ctx context.Context, opts agentInstallOptions) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("hyperv agent install is only supported on Windows")
	}
	bin := opts.Binary
	if bin == "" {
		bin = filepath.Join(DefaultAgentDataDir, "hyperv-agent.exe")
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("agent binary %q not found: set hyperv.agentBinary to the hyperv-agent.exe path (release-asset download is not wired yet): %w", bin, err)
	}
	token := opts.Token
	if token == "" {
		t, err := mintToken()
		if err != nil {
			return err
		}
		token = t
	}
	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = DefaultAgentDataDir
	}
	host := opts.Endpoint
	if h, _, err := net.SplitHostPort(opts.Endpoint); err == nil && h != "" {
		host = h
	}
	args := []string{"install", "--token", token, "--data-dir", dataDir}
	if opts.Endpoint != "" {
		args = append(args, "--listen", opts.Endpoint, "--tls-hosts", host)
	}
	if opts.Out != nil {
		_, _ = fmt.Fprintf(opts.Out, "installing hyperv-agent service from %s (listen %s)\n", bin, opts.Endpoint)
	}
	if err := runMaybeElevated(ctx, bin, args); err != nil {
		return fmt.Errorf("installing hyperv-agent: %w", err)
	}
	return nil
}

// runMaybeElevated runs bin with args directly when already elevated, else
// relaunches it through PowerShell Start-Process -Verb RunAs for a UAC prompt.
func runMaybeElevated(ctx context.Context, bin string, args []string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err == nil {
		return nil
	} else if !isAccessDenied(err) {
		return err
	}
	// Elevate. Quote each argument for PowerShell's argument list.
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", "''") + "'"
	}
	ps := fmt.Sprintf("Start-Process -FilePath '%s' -Verb RunAs -Wait -ArgumentList %s", bin, strings.Join(quoted, ","))
	elevated := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)
	elevated.Stdout, elevated.Stderr = os.Stderr, os.Stderr
	return elevated.Run()
}

// isAccessDenied reports whether err looks like a Windows access-denied (so we
// should retry with elevation).
func isAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "access is denied") || strings.Contains(s, "exit status 5")
}

// countHyperVClusters returns how many klotho-tracked clusters use the hyperv
// provider. The host agent is host-scoped shared infrastructure - one daemon
// serves every klotho Hyper-V cluster on the box - so this count is the basis
// for the "agent left running" note on cluster delete and the safety check on
// agent uninstall. Reserved and unreadable records are not counted.
func countHyperVClusters(ctx context.Context, deps klotho.Deps) (int, error) {
	sums, err := klotho.List(ctx, deps)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range sums {
		if s.Error == "" && !s.Reserved && s.Provider == providerHyperv {
			n++
		}
	}
	return n, nil
}

// noteHyperVDaemonAfterDelete prints, after a successful Hyper-V cluster
// delete, that the shared host agent was intentionally left running - deleting
// a cluster never touches the daemon. It counts the Hyper-V clusters that
// remain so the note says whether others still need it, and, when none do,
// points at the explicit uninstall command. It is informational only and never
// removes anything.
func noteHyperVDaemonAfterDelete(ctx context.Context, w interface{ Write([]byte) (int, error) }, deps klotho.Deps) {
	n, err := countHyperVClusters(ctx, deps)
	if err != nil {
		_, _ = fmt.Fprintln(w, "Hyper-V agent left running (it is shared across every Hyper-V cluster on this host).")
		return
	}
	if n > 0 {
		_, _ = fmt.Fprintf(w, "Hyper-V agent left running - shared; %d other Hyper-V cluster(s) still use it.\n", n)
		return
	}
	_, _ = fmt.Fprintln(w, "Hyper-V agent left running. No other Hyper-V clusters remain; run \"klotho hyperv-agent uninstall\" when you're done with this host.")
}

// newHyperVAgentCommand is "klotho hyperv-agent", with install and uninstall
// subcommands that reuse the create path's install logic.
func newHyperVAgentCommand() *cli.Command {
	return &cli.Command{
		Name:  "hyperv-agent",
		Usage: "Manage the Hyper-V host agent service (Windows)",
		Commands: []*cli.Command{
			{
				Name:  "install",
				Usage: "Install and start the Hyper-V host agent Windows service",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "endpoint", Usage: "host:port the agent binds", Value: DefaultAgentEndpoint},
					&cli.StringFlag{Name: "token", Usage: "bearer token (default: a fresh random token)"},
					&cli.StringFlag{Name: "binary", Usage: "path to hyperv-agent.exe (default: hyperv.agentBinary, then the install dir)"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					a, err := newApp(c)
					if err != nil {
						return err
					}
					bin := firstNonEmpty(c.String("binary"), a.cfg.Hyperv.AgentBinary)
					return installHyperVAgent(ctx, agentInstallOptions{
						Endpoint: c.String("endpoint"),
						Token:    c.String("token"),
						Binary:   bin,
						Out:      stderr(c),
					})
				},
			},
			{
				Name: "uninstall",
				Usage: "Stop and remove the Hyper-V host agent Windows service. The agent is " +
					"shared across every klotho Hyper-V cluster on this host, so uninstall " +
					"refuses while any cluster still uses it unless --force is given.",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "binary", Usage: "path to hyperv-agent.exe"},
					&cli.BoolFlag{Name: "force", Usage: "remove the agent even while klotho Hyper-V clusters still use it"},
					&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					a, err := newApp(c)
					if err != nil {
						return err
					}
					if runtime.GOOS != "windows" {
						return fmt.Errorf("hyperv agent uninstall is only supported on Windows")
					}
					// Safety check: the agent is host-scoped shared infrastructure.
					// Never uninstall it out from under clusters that still need it.
					// This is an explicit host-level action; a cluster delete never
					// reaches here.
					if !c.Bool("force") {
						switch n, cerr := countHyperVClusters(ctx, a.deps()); {
						case cerr != nil:
							_, _ = fmt.Fprintf(stderr(c), "warning: could not check for Hyper-V clusters still using the agent: %v\n", cerr)
						case n == 0:
							// No cluster depends on it; proceed.
						case c.Bool("yes"):
							_, _ = fmt.Fprintf(stderr(c), "warning: %d klotho Hyper-V cluster(s) still use the shared agent; removing it anyway (--yes)\n", n)
						case interactive(c):
							_, _ = fmt.Fprintf(stderr(c), "%d klotho Hyper-V cluster(s) still use the shared Hyper-V agent. Remove it anyway? [y/N] ", n)
							line, _ := bufio.NewReader(stdin(c)).ReadString('\n')
							switch strings.ToLower(strings.TrimSpace(line)) {
							case "y", "yes":
							default:
								return fmt.Errorf("aborted: the hyperv-agent is still used by %d klotho Hyper-V cluster(s)", n)
							}
						default:
							return fmt.Errorf("refusing to uninstall the hyperv-agent: %d klotho Hyper-V cluster(s) still use it; delete them first, or pass --force to remove it anyway", n)
						}
					}
					bin := firstNonEmpty(c.String("binary"), a.cfg.Hyperv.AgentBinary, filepath.Join(DefaultAgentDataDir, "hyperv-agent.exe"))
					if _, err := os.Stat(bin); err != nil {
						// Fall back to sc.exe delete when the exe is gone.
						return runMaybeElevated(ctx, "sc.exe", []string{"delete", agentServiceName})
					}
					return runMaybeElevated(ctx, bin, []string{"uninstall"})
				},
			},
		},
	}
}
