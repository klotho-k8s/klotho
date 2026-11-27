package klotho

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/credential"
	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/pkg/kube"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// clusterContractVersion is the Cluster API contract version klotho targets;
// the Cluster and Machine objects it deletes and lists carry it.
const clusterContractVersion = "v1beta1"

var (
	clusterGVK = schema.GroupVersionKind{Group: generate.ClusterGroup, Version: clusterContractVersion, Kind: "Cluster"}
	machineGVK = schema.GroupVersionKind{Group: generate.ClusterGroup, Version: clusterContractVersion, Kind: "Machine"}
)

// adminCredentialTTL bounds the admin certificate minted to drive a deletion.
const adminCredentialTTL = time.Hour

// errClusterUnreachable marks a connection failure so Delete falls back to
// removing the infrastructure directly.
var errClusterUnreachable = errors.New("cluster API not reachable")

// connectAdmin opens the cluster's API with an admin credential minted from
// its state. deps.ConnectAdmin overrides it; otherwise it mints a client
// certificate from state.CA, builds a kubeconfig for state.Endpoint, connects
// (via deps.Connect, or kube.New), and probes /readyz. A connect or readiness
// failure is wrapped with errClusterUnreachable.
func (deps Deps) connectAdmin(ctx context.Context, st *cluster.State) (kube.Cluster, error) {
	if deps.ConnectAdmin != nil {
		return deps.ConnectAdmin(ctx, st)
	}
	certPEM, keyPEM, err := credential.Mint(st.CA, st.Spec.Name, adminCredentialTTL)
	if err != nil {
		return nil, fmt.Errorf("minting admin credential for %s: %w", st.Spec.Name, err)
	}
	cfg := clientcmdapi.NewConfig()
	name := st.Spec.Name
	// Dial the host-reachable client endpoint when the provider reported one
	// (CAPD on rootless podman), else the cluster endpoint.
	dialEP := st.Endpoint
	if st.ClientEndpoint.Host != "" {
		dialEP = st.ClientEndpoint
	}
	cfg.Clusters[name] = &clientcmdapi.Cluster{
		Server:                   credential.ServerURL(dialEP),
		CertificateAuthorityData: st.CA.Cert,
	}
	user := "klotho-admin@" + name
	cfg.AuthInfos[user] = &clientcmdapi.AuthInfo{
		ClientCertificateData: certPEM,
		ClientKeyData:         keyPEM,
	}
	cfg.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: user}
	cfg.CurrentContext = name
	kubeconfig, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, fmt.Errorf("writing admin kubeconfig for %s: %w", name, err)
	}
	connect := deps.Connect
	if connect == nil {
		connect = kube.New
	}
	cl, err := connect(ctx, kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errClusterUnreachable, err)
	}
	if err := cl.Ready(ctx); err != nil {
		return nil, fmt.Errorf("%w: %w", errClusterUnreachable, err)
	}
	return cl, nil
}

// deleteDirect removes the infrastructure by tag and the record, without
// Cluster API. It is the emergency path and the fallback when the cluster's
// API is gone.
func deleteDirect(ctx context.Context, deps Deps, name string, st *cluster.State, plan *infrastructure.Plan, log *slog.Logger) error {
	log.Info("tearing down", "resources", len(plan.Resources))
	if err := deps.Infra.Teardown(ctx, st, plan); err != nil {
		return fmt.Errorf("tearing down %s: %w", name, err)
	}
	if deps.Payload != nil {
		if err := deps.Payload.Remove(ctx, name); err != nil {
			return fmt.Errorf("removing payload of %s: %w", name, err)
		}
	}
	if err := deps.Store.Delete(ctx, name); err != nil {
		return fmt.Errorf("removing record of %s: %w", name, err)
	}
	log.Info("deleted")
	return nil
}

// deleteViaClusterAPI arms any server-side cleanup, deletes the Cluster object,
// and either returns (fire-and-forget) or watches the deletion through and
// sweeps the remainder.
func deleteViaClusterAPI(ctx context.Context, deps Deps, name string, st *cluster.State, cl kube.Cluster, opts DeleteOptions, log *slog.Logger) error {
	log.Info("deleting the Cluster object; Cluster API removes the machines in order")
	if err := cl.Delete(ctx, clusterGVK, "default", name); err != nil {
		return fmt.Errorf("deleting Cluster %s: %w", name, err)
	}

	if !opts.watch() {
		if sc, ok := deps.Infra.(infrastructure.DeletionSidecar); ok {
			log.Info("launching the deletion sidecar to finish the teardown after klotho exits")
			if err := sc.LaunchDeletionSidecar(ctx, st); err != nil {
				return fmt.Errorf("launching deletion sidecar for %s: %w", name, err)
			}
		} else {
			log.Info("fire-and-forget: Cluster API is deleting the cluster and finishes the last machine itself")
		}
		if deps.Payload != nil {
			if err := deps.Payload.Remove(ctx, name); err != nil {
				return fmt.Errorf("removing payload of %s: %w", name, err)
			}
		}
		// Clear the state record now that the deletion is under way. The Cluster
		// API (and any detached sidecar) finishes the teardown on its own; the
		// record must not linger in `klotho list` after a fire-and-forget delete.
		if err := deps.Store.Delete(ctx, name); err != nil {
			return fmt.Errorf("removing record of %s: %w", name, err)
		}
		log.Info("deleted")
		return nil
	}

	if err := watchDeletion(ctx, deps, st, cl, opts, log); err != nil {
		return err
	}

	plan, err := deps.Infra.Plan(ctx, st)
	if err != nil {
		return fmt.Errorf("planning sweep of %s: %w", name, err)
	}
	log.Info("sweeping leftovers", "resources", len(plan.Resources))
	if err := deps.Infra.Teardown(ctx, st, plan); err != nil {
		return fmt.Errorf("tearing down %s: %w", name, err)
	}
	if deps.Payload != nil {
		if err := deps.Payload.Remove(ctx, name); err != nil {
			return fmt.Errorf("removing payload of %s: %w", name, err)
		}
	}
	if err := deps.Store.Delete(ctx, name); err != nil {
		return fmt.Errorf("removing record of %s: %w", name, err)
	}
	log.Info("deleted")
	return nil
}

// watchDeletion polls the provider's teardown plan until nothing of the
// cluster is left in the infrastructure, or the watch times out. A timeout is
// not an error: the caller sweeps whatever remains.
func watchDeletion(ctx context.Context, deps Deps, st *cluster.State, cl kube.Cluster, opts DeleteOptions, log *slog.Logger) error {
	deadline := deps.now().Add(opts.watchTimeout())
	for {
		plan, err := deps.Infra.Plan(ctx, st)
		if err != nil {
			return fmt.Errorf("watching deletion of %s: %w", st.Spec.Name, err)
		}
		remaining := len(plan.Resources)
		log.Info("waiting for Cluster API to delete the cluster", "resources", remaining, "machines", machineCount(ctx, cl))
		if remaining == 0 {
			return nil
		}
		if !deps.now().Before(deadline) {
			log.Warn("timed out watching deletion; sweeping the rest", "resources", remaining)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(deps.poll()):
		}
	}
}

// machineCount reports how many Machines remain, best effort, for a progress
// line. A read failure returns -1 rather than failing the watch.
func machineCount(ctx context.Context, cl kube.Cluster) int {
	ms, err := cl.List(ctx, machineGVK, "")
	if err != nil {
		return -1
	}
	return len(ms)
}
