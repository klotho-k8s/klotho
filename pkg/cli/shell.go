package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/klotho-k8s/klotho"
	"github.com/klotho-k8s/klotho/pkg/credential"
)

// EnvCluster names the cluster a klotho shell is for.
const EnvCluster = "KLOTHO_CLUSTER"

func newShellCommand() *cli.Command {
	return &cli.Command{
		Name:      "shell",
		Usage:     "Open a shell with KUBECONFIG set for a cluster, or run one command in it",
		ArgsUsage: "<name> [-- command [args...]]",
		Description: "The kubeconfig holds no credential: it runs \"klotho credential\", which mints a\n" +
			"short-lived client certificate from the cluster's state on demand.",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "print",
				Usage: "write the kubeconfig and print an export KUBECONFIG=... line instead of starting a shell",
			},
		},
		Action: runShell,
	}
}

func runShell(ctx context.Context, c *cli.Command) error {
	name, err := clusterNameArg(c)
	if err != nil {
		return err
	}
	command := c.Args().Slice()[1:]
	a, err := newApp(c)
	if err != nil {
		return err
	}
	st, err := klotho.Load(ctx, a.deps(), name)
	if err != nil {
		return err
	}
	if st.Reserved {
		return fmt.Errorf("cluster %s is only reserved; there is nothing to connect to", name)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("shell: locating the klotho binary: %w", err)
	}
	kubeconfig, err := credential.Kubeconfig(name, st.Endpoint, st.CA.Cert, exe, a.credentialArgs(c))
	if err != nil {
		return err
	}
	dir := a.cfg.ShellDir()
	if c.Bool("print") {
		if err := writePrivate(dir, name+".kubeconfig", kubeconfig); err != nil {
			return err
		}
		path := filepath.Join(dir, name+".kubeconfig")
		_, _ = fmt.Fprintf(stdout(c), "export KUBECONFIG=%s\n", shellQuote(path))
		return nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, name+"-*.kubeconfig")
	if err != nil {
		return err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err := f.Write(kubeconfig); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if len(command) == 0 {
		sh := interactiveShell()
		_, _ = fmt.Fprintf(stderr(c), "klotho: shell for cluster %s (KUBECONFIG=%s); exit to leave\n", name, path)
		command = sh
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+path, EnvCluster+"="+name)
	cmd.Stdin = stdin(c)
	cmd.Stdout = stdout(c)
	cmd.Stderr = stderr(c)
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// The deferred removal runs before the exit handler exits.
		return cli.Exit("", exitErr.ExitCode())
	}
	return err
}

// interactiveShell is $SHELL, or on Windows PowerShell when it is on PATH
// and %COMSPEC% otherwise, or /bin/sh.
func interactiveShell() []string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return []string{sh}
	}
	if runtime.GOOS == "windows" {
		for _, ps := range []string{"pwsh.exe", "powershell.exe"} {
			if p, err := exec.LookPath(ps); err == nil {
				return []string{p, "-NoLogo"}
			}
		}
		if cs := os.Getenv("COMSPEC"); cs != "" {
			return []string{cs}
		}
		return []string{"cmd.exe"}
	}
	return []string{"/bin/sh"}
}

// shellQuote quotes s as a single POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
