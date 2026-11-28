package vsphere

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

func buildPlan(res ...cluster.Resource) *infrastructure.Plan {
	return &infrastructure.Plan{Resources: res}
}

// machineTemplate builds a VSphereMachineTemplate with the given name and source
// template.
func machineTemplate(name, sourceTemplate string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"server":     "vcenter.example.com",
					"template":   sourceTemplate,
					"datacenter": "dc1",
					"datastore":  "datastore1",
					"cloneMode":  "fullClone",
					"numCPUs":    int64(2),
					"memoryMiB":  int64(8192),
				},
			},
		},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
	u.SetKind("VSphereMachineTemplate")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

// kcp builds a KubeadmControlPlane referencing the control-plane machine
// template.
func kcp(cluster string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"machineTemplate": map[string]any{
				"spec": map[string]any{
					"infrastructureRef": map[string]any{
						"name": cluster + "-control-plane",
					},
				},
			},
		},
	}}
	u.SetAPIVersion("controlplane.cluster.x-k8s.io/v1beta2")
	u.SetKind("KubeadmControlPlane")
	u.SetName(cluster + "-control-plane")
	u.SetNamespace("default")
	return u
}

// vsphereCluster builds a VSphereCluster with a server and endpoint.
func vsphereCluster(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"server":     "vcenter.example.com",
			"thumbprint": "AA:BB:CC",
			"controlPlaneEndpoint": map[string]any{
				"host": "192.168.1.200",
				"port": int64(6443),
			},
			"identityRef": map[string]any{
				"kind": "Secret",
				"name": name,
			},
		},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1beta2")
	u.SetKind("VSphereCluster")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

// controlPlaneBundle is a minimal bundle for Adopt: a control-plane machine
// template and the KubeadmControlPlane that references it.
func controlPlaneBundle() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		machineTemplate("lab1-control-plane", "ubuntu-2404-template"),
		kcp("lab1"),
	}
}

// fullBundle is a complete flat single-node bundle for Check.
func fullBundle() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		vsphereCluster("lab1"),
		machineTemplate("lab1-control-plane", "ubuntu-2404-template"),
		kcp("lab1"),
	}
}
