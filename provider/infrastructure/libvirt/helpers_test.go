package libvirt

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

func buildPlan(res ...cluster.Resource) *infrastructure.Plan {
	return &infrastructure.Plan{Resources: res}
}

// machineTemplate builds a LibvirtMachineTemplate with the given name and image.
func machineTemplate(name, image string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"image":       image,
					"storagePool": "default",
					"network":     map[string]any{"name": "default", "type": "network"},
				},
			},
		},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1alpha1")
	u.SetKind("LibvirtMachineTemplate")
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

// libvirtCluster builds a LibvirtCluster with a connection URI and endpoint.
func libvirtCluster(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"connectionURI": "qemu:///system",
			"controlPlaneEndpoint": map[string]any{
				"host": "192.168.122.10",
				"port": int64(6443),
			},
		},
	}}
	u.SetAPIVersion("infrastructure.cluster.x-k8s.io/v1alpha1")
	u.SetKind("LibvirtCluster")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

// controlPlaneBundle is a minimal bundle for Adopt: a control-plane machine
// template and the KubeadmControlPlane that references it.
func controlPlaneBundle() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		machineTemplate("lab1-control-plane", "base.qcow2"),
		kcp("lab1"),
	}
}

// fullBundle is a complete flat single-node bundle for Check.
func fullBundle() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		libvirtCluster("lab1"),
		machineTemplate("lab1-control-plane", "base.qcow2"),
		kcp("lab1"),
	}
}
