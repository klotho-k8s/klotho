package generate

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// API groups the helpers look objects up in.
const (
	ClusterGroup        = "cluster.x-k8s.io"
	InfrastructureGroup = "infrastructure.cluster.x-k8s.io"
)

// Bundle is a rendered cluster manifest. YAML and Objects describe the same
// documents; after changing Objects directly, call Render to bring YAML back
// in line. The patch helpers in this package do that themselves.
type Bundle struct {
	// YAML is the multi-document manifest exactly as it will be applied.
	YAML []byte
	// Objects are the parsed documents, in document order.
	Objects []*unstructured.Unstructured
	// Variables are the resolved variable values that were used. It is nil
	// for a bundle built by ParseBundle.
	Variables map[string]string
}

// ParseBundle builds a Bundle from a multi-document YAML manifest. Empty
// documents are skipped; document order is kept. The returned YAML is the
// re-rendered form of the parsed objects.
func ParseBundle(data []byte) (*Bundle, error) {
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	b := &Bundle{}
	for i := 0; ; i++ {
		doc, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.Wrapf(err, "reading document %d", i)
		}
		js, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, errors.Wrapf(err, "decoding document %d", i)
		}
		js = bytes.TrimSpace(js)
		if len(js) == 0 || string(js) == "null" || string(js) == "{}" {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(js); err != nil {
			return nil, errors.Wrapf(err, "decoding document %d", i)
		}
		b.Objects = append(b.Objects, obj)
	}
	if _, err := b.Render(); err != nil {
		return nil, err
	}
	return b, nil
}

// Render serializes Objects into YAML, separated by "---", stores the result
// in b.YAML and returns it.
func (b *Bundle) Render() ([]byte, error) {
	var buf bytes.Buffer
	for i, obj := range b.Objects {
		data, err := yaml.Marshal(obj.Object)
		if err != nil {
			return nil, errors.Wrapf(err, "serializing %s %q", obj.GetKind(), obj.GetName())
		}
		if i > 0 {
			buf.WriteString("---\n")
		}
		buf.Write(data)
	}
	b.YAML = buf.Bytes()
	return b.YAML, nil
}

// Find returns every object in group whose kind ends with kindSuffix, in
// document order.
func Find(b *Bundle, group, kindSuffix string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, obj := range b.Objects {
		gvk := obj.GroupVersionKind()
		if gvk.Group == group && strings.HasSuffix(gvk.Kind, kindSuffix) {
			out = append(out, obj)
		}
	}
	return out
}

// FindOne returns the single object in group whose kind ends with kindSuffix,
// and fails when there are none or more than one.
func FindOne(b *Bundle, group, kindSuffix string) (*unstructured.Unstructured, error) {
	found := Find(b, group, kindSuffix)
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return nil, fmt.Errorf("no %s object with kind *%s", group, kindSuffix)
	default:
		return nil, fmt.Errorf("expected one %s object with kind *%s, found %d: %s",
			group, kindSuffix, len(found), describe(found))
	}
}

// SetControlPlaneEndpoint sets spec.controlPlaneEndpoint on the single
// infrastructure cluster object and re-renders b.YAML.
func SetControlPlaneEndpoint(b *Bundle, ep cluster.Endpoint) error {
	obj, err := FindOne(b, InfrastructureGroup, "Cluster")
	if err != nil {
		return err
	}
	if err := unstructured.SetNestedField(obj.Object, ep.Host, "spec", "controlPlaneEndpoint", "host"); err != nil {
		return errors.Wrapf(err, "setting endpoint host on %s %q", obj.GetKind(), obj.GetName())
	}
	if err := unstructured.SetNestedField(obj.Object, int64(ep.Port), "spec", "controlPlaneEndpoint", "port"); err != nil {
		return errors.Wrapf(err, "setting endpoint port on %s %q", obj.GetKind(), obj.GetName())
	}
	_, err = b.Render()
	return err
}

// AddMachineTemplateTag appends tag to spec.template.spec.tags of every
// infrastructure MachineTemplate that lacks it, creating the list when
// missing, and re-renders b.YAML.
func AddMachineTemplateTag(b *Bundle, tag string) error {
	templates := Find(b, InfrastructureGroup, "MachineTemplate")
	if len(templates) == 0 {
		return fmt.Errorf("no %s MachineTemplate objects", InfrastructureGroup)
	}
	for _, obj := range templates {
		tags, _, err := unstructured.NestedStringSlice(obj.Object, "spec", "template", "spec", "tags")
		if err != nil {
			return errors.Wrapf(err, "reading tags of %s %q", obj.GetKind(), obj.GetName())
		}
		if slices.Contains(tags, tag) {
			continue
		}
		if err := unstructured.SetNestedStringSlice(obj.Object, append(tags, tag), "spec", "template", "spec", "tags"); err != nil {
			return errors.Wrapf(err, "setting tags of %s %q", obj.GetKind(), obj.GetName())
		}
	}
	_, err := b.Render()
	return err
}

// SetMachineTemplatePool sets spec.template.spec.pool on every infrastructure
// MachineTemplate and re-renders b.YAML.
func SetMachineTemplatePool(b *Bundle, pool string) error {
	templates := Find(b, InfrastructureGroup, "MachineTemplate")
	if len(templates) == 0 {
		return fmt.Errorf("no %s MachineTemplate objects", InfrastructureGroup)
	}
	for _, obj := range templates {
		if err := unstructured.SetNestedField(obj.Object, pool, "spec", "template", "spec", "pool"); err != nil {
			return errors.Wrapf(err, "setting pool of %s %q", obj.GetKind(), obj.GetName())
		}
	}
	_, err := b.Render()
	return err
}

// Validate checks a user-supplied manifest: exactly one Cluster named
// clusterName, exactly one infrastructure cluster object, at least one
// infrastructure MachineTemplate, and requiredTag on every MachineTemplate.
// All problems are reported together.
func Validate(b *Bundle, clusterName, requiredTag string) error {
	var problems []error

	var clusters []*unstructured.Unstructured
	for _, obj := range Find(b, ClusterGroup, "Cluster") {
		if obj.GetKind() == "Cluster" {
			clusters = append(clusters, obj)
		}
	}
	switch len(clusters) {
	case 0:
		problems = append(problems, fmt.Errorf("no %s Cluster object", ClusterGroup))
	case 1:
		if got := clusters[0].GetName(); got != clusterName {
			problems = append(problems, fmt.Errorf("cluster is named %q, expected %q", got, clusterName))
		}
	default:
		problems = append(problems, fmt.Errorf("expected one %s Cluster object, found %d: %s",
			ClusterGroup, len(clusters), describe(clusters)))
	}

	if _, err := FindOne(b, InfrastructureGroup, "Cluster"); err != nil {
		problems = append(problems, err)
	}

	templates := Find(b, InfrastructureGroup, "MachineTemplate")
	if len(templates) == 0 {
		problems = append(problems, fmt.Errorf("no %s MachineTemplate objects", InfrastructureGroup))
	}
	for _, obj := range templates {
		tags, _, err := unstructured.NestedStringSlice(obj.Object, "spec", "template", "spec", "tags")
		if err != nil {
			problems = append(problems, errors.Wrapf(err, "reading tags of %s %q", obj.GetKind(), obj.GetName()))
			continue
		}
		if !slices.Contains(tags, requiredTag) {
			problems = append(problems, fmt.Errorf("%s %q lacks tag %q in spec.template.spec.tags",
				obj.GetKind(), obj.GetName(), requiredTag))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: problems}
}

// ValidationError aggregates every problem Validate found.
type ValidationError struct {
	Problems []error
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		msgs[i] = p.Error()
	}
	return "invalid manifest: " + strings.Join(msgs, "; ")
}

// Unwrap exposes the individual problems to errors.Is and errors.As.
func (e *ValidationError) Unwrap() []error {
	return e.Problems
}

func describe(objs []*unstructured.Unstructured) string {
	names := make([]string, len(objs))
	for i, o := range objs {
		names[i] = o.GetKind() + "/" + o.GetName()
	}
	return strings.Join(names, ", ")
}

// SetClusterNetwork sets spec.clusterNetwork.pods.cidrBlocks and/or
// spec.clusterNetwork.services.cidrBlocks on the Cluster object (an empty
// slice leaves that network as it is) and re-renders b.YAML.
func SetClusterNetwork(b *Bundle, pods, services []string) error {
	obj, err := FindOne(b, ClusterGroup, "Cluster")
	if err != nil {
		return err
	}
	if len(pods) > 0 {
		if err := unstructured.SetNestedStringSlice(obj.Object, pods, "spec", "clusterNetwork", "pods", "cidrBlocks"); err != nil {
			return errors.Wrapf(err, "setting pod CIDRs on %s %q", obj.GetKind(), obj.GetName())
		}
	}
	if len(services) > 0 {
		if err := unstructured.SetNestedStringSlice(obj.Object, services, "spec", "clusterNetwork", "services", "cidrBlocks"); err != nil {
			return errors.Wrapf(err, "setting service CIDRs on %s %q", obj.GetKind(), obj.GetName())
		}
	}
	_, err = b.Render()
	return err
}

// ClusterNetwork reads the pod and service CIDR blocks of a Cluster object.
// Missing networks come back as nil slices.
func ClusterNetwork(obj *unstructured.Unstructured) (pods, services []string, err error) {
	if obj == nil {
		return nil, nil, nil
	}
	pods, _, err = unstructured.NestedStringSlice(obj.Object, "spec", "clusterNetwork", "pods", "cidrBlocks")
	if err != nil {
		return nil, nil, errors.Wrapf(err, "reading pod CIDRs of %s %q", obj.GetKind(), obj.GetName())
	}
	services, _, err = unstructured.NestedStringSlice(obj.Object, "spec", "clusterNetwork", "services", "cidrBlocks")
	if err != nil {
		return nil, nil, errors.Wrapf(err, "reading service CIDRs of %s %q", obj.GetKind(), obj.GetName())
	}
	return pods, services, nil
}
