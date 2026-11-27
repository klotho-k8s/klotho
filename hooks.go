package klotho

import (
	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/hooks"
)

// hookExec returns the Executor to run hooks with: the override when set,
// otherwise the built-in engine logging through Deps' logger.
func (d Deps) hookExec(override hooks.Executor) hooks.Executor {
	if override != nil {
		return override
	}
	return hooks.NewEngine(d.log())
}

// hookTarget describes the booted cluster to the hooks: its name, endpoint,
// kube context and an admin kubeconfig an exec hook can point KUBECONFIG at.
// The kube context and user are named for the cluster, as adminKubeconfig
// writes them.
func hookTarget(st *cluster.State) (hooks.Target, error) {
	kubeconfig, err := adminKubeconfig(st)
	if err != nil {
		return hooks.Target{}, err
	}
	return hooks.Target{
		Cluster:     st.Spec.Name,
		Endpoint:    st.Endpoint,
		KubeContext: st.Spec.Name,
		Kubeconfig:  kubeconfig,
	}, nil
}
