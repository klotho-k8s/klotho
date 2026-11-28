package k8ssecret

import (
	"fmt"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// newClient builds a typed Kubernetes clientset for the target cluster from
// cfg. It follows the standard kubeconfig loading rules (an explicit path wins,
// else KUBECONFIG, else ~/.kube/config) and selects a context when one is
// given, falling back to the kubeconfig's current-context. Building the client
// does not reach the cluster; the first request does.
func newClient(cfg Config) (kubernetes.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.Kubeconfig != "" {
		rules.ExplicitPath = cfg.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if cfg.Context != "" {
		overrides.CurrentContext = cfg.Context
	}
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes backend: load kubeconfig: %w", err)
	}
	restCfg.UserAgent = "klotho"
	restCfg.Timeout = 30 * time.Second
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes backend: build client: %w", err)
	}
	return client, nil
}
