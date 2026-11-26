package docker

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/klotho-k8s/klotho/pkg/generate"
	"github.com/klotho-k8s/klotho/provider"
)

// Check reviews the generated bundle. It implements provider.Checker. The
// findings are advisory: klotho never edits the bundle to resolve one.
func (p *Provider) Check(bundle []*unstructured.Unstructured) []provider.Finding {
	b := &generate.Bundle{Objects: bundle}
	var findings []provider.Finding

	dc, err := generate.FindOne(b, generate.InfrastructureGroup, "DockerCluster")
	if err != nil {
		findings = append(findings, provider.Finding{
			Object:  "DockerCluster",
			Message: "the bundle has no DockerCluster; the Docker provider expects a CAPD (development flavor) template.",
		})
		return findings
	}

	findings = append(findings, provider.Finding{
		Object:  provider.ObjectRef(dc),
		Message: "the Cluster API Docker provider (CAPD) is for development and testing only, not production workloads.",
	})
	return findings
}
