package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// Environment variables an exec hook receives, on top of the parent process's
// environment. KUBECONFIG points at a private temporary file holding the
// cluster's admin kubeconfig; the rest are metadata so a hook need not parse
// the kubeconfig to learn where it is pointed.
const (
	// EnvKubeconfig is the standard variable kubectl, helm and flux read.
	EnvKubeconfig = "KUBECONFIG"
	// EnvCluster is the cluster's name.
	EnvCluster = "KLOTHO_CLUSTER"
	// EnvEvent is the lifecycle event being fired.
	EnvEvent = "KLOTHO_HOOK_EVENT"
	// EnvHookName is the name of the hook being run.
	EnvHookName = "KLOTHO_HOOK_NAME"
	// EnvEndpoint is the API server endpoint as host:port.
	EnvEndpoint = "KLOTHO_CLUSTER_ENDPOINT"
	// EnvHost is the API server host.
	EnvHost = "KLOTHO_CLUSTER_HOST"
	// EnvPort is the API server port.
	EnvPort = "KLOTHO_CLUSTER_PORT"
	// EnvContext is the context name inside the kubeconfig.
	EnvContext = "KLOTHO_KUBECONTEXT"
)

// ErrApplyNotImplemented is returned for an apply hook. The apply type is wired
// through the configuration and the dispatch, but the engine does not run it
// yet; an exec hook running "kubectl apply -f" covers the same ground.
var ErrApplyNotImplemented = errors.New("hooks: the apply type is not implemented yet; use an exec hook that runs kubectl apply")

// Engine is the default Executor. It runs exec hooks as child processes and
// streams their output into the logger.
type Engine struct {
	log *slog.Logger
}

// NewEngine returns the default Executor, logging hook output through log.
func NewEngine(log *slog.Logger) Engine {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return Engine{log: log}
}

// Run runs one hook. It is the default Executor implementation.
func (e Engine) Run(ctx context.Context, h Hook, t Target) error {
	switch h.Type {
	case Exec, "":
		return e.runExec(ctx, h, t)
	case Apply:
		return ErrApplyNotImplemented
	default:
		return fmt.Errorf("unknown hook type %q", h.Type)
	}
}

// runExec writes the kubeconfig to a private temporary file, runs the command
// with KUBECONFIG and the cluster metadata in its environment, and streams its
// combined output into the logger. The command inherits the parent
// environment so a consumer's tokens and settings are available.
func (e Engine) runExec(ctx context.Context, h Hook, t Target) error {
	if h.Command == "" {
		return errors.New("exec hook has no command")
	}

	path, cleanup, err := writeKubeconfig(t.Kubeconfig)
	if err != nil {
		return err
	}
	defer cleanup()

	cmd := exec.CommandContext(ctx, h.Command, h.Args...)
	cmd.Env = append(os.Environ(), hookEnv(h, t, path)...)
	out := &lineWriter{log: e.log, name: h.Name}
	cmd.Stdout = out
	cmd.Stderr = out
	runErr := cmd.Run()
	out.flush()
	return runErr
}

// writeKubeconfig writes kubeconfig to a private temporary file and returns its
// path and a cleanup func. An empty kubeconfig still yields a (empty) file so
// KUBECONFIG is always set to a real path.
func writeKubeconfig(kubeconfig []byte) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "klotho-hook-*.kubeconfig")
	if err != nil {
		return "", func() {}, fmt.Errorf("hooks: writing kubeconfig: %w", err)
	}
	path = f.Name()
	cleanup = func() { _ = os.Remove(path) }
	if _, err := f.Write(kubeconfig); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("hooks: writing kubeconfig: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("hooks: writing kubeconfig: %w", err)
	}
	return path, cleanup, nil
}

// hookEnv is the KUBECONFIG and KLOTHO_* variables an exec hook receives.
func hookEnv(h Hook, t Target, kubeconfigPath string) []string {
	return []string{
		EnvKubeconfig + "=" + kubeconfigPath,
		EnvCluster + "=" + t.Cluster,
		EnvEvent + "=" + string(t.Event),
		EnvHookName + "=" + h.Name,
		EnvEndpoint + "=" + endpointHostPort(t.Endpoint),
		EnvHost + "=" + t.Endpoint.Host,
		EnvPort + "=" + strconv.Itoa(endpointPort(t.Endpoint)),
		EnvContext + "=" + t.KubeContext,
	}
}

func endpointPort(ep cluster.Endpoint) int {
	if ep.Port == 0 {
		return 6443
	}
	return ep.Port
}

func endpointHostPort(ep cluster.Endpoint) string {
	if ep.Host == "" {
		return ""
	}
	return net.JoinHostPort(ep.Host, strconv.Itoa(endpointPort(ep)))
}

// lineWriter forwards a child process's output to a logger, one record per
// line, so hook output is captured in klotho's logs as it is produced.
type lineWriter struct {
	log  *slog.Logger
	name string
	buf  bytes.Buffer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := string(w.buf.Next(i + 1))
		w.log.Info("hook output", "name", w.name, "line", trimEOL(line))
	}
	return len(p), nil
}

// flush logs any output not terminated by a newline.
func (w *lineWriter) flush() {
	if w.buf.Len() == 0 {
		return
	}
	w.log.Info("hook output", "name", w.name, "line", trimEOL(w.buf.String()))
	w.buf.Reset()
}

func trimEOL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
