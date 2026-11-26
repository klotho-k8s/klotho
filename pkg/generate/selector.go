package generate

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Template match policies of an infrastructure MachineTemplate's template
// selector. They are the values the Cluster API Proxmox provider accepts in
// spec.template.spec.templateMatchPolicy.
const (
	// TemplateMatchExact selects the template whose tags equal the selector's
	// tags one to one. It is the default.
	TemplateMatchExact = "exact"
	// TemplateMatchUniqueSubset selects the single template that carries
	// every selector tag; extra tags are allowed, but only one template may
	// match.
	TemplateMatchUniqueSubset = "uniqueSubset"
	// TemplateMatchBestSubset selects, among the templates that carry every
	// selector tag, the one with the fewest extra tags.
	TemplateMatchBestSubset = "bestSubset"
)

// TemplateMatchPolicies lists the valid policies, default first.
var TemplateMatchPolicies = []string{TemplateMatchExact, TemplateMatchUniqueSubset, TemplateMatchBestSubset}

// TemplateTagPattern is what a Proxmox tag may look like.
var TemplateTagPattern = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)

// NormalizeTemplateMatchPolicy returns policy with the default applied, and
// fails for an unknown one.
func NormalizeTemplateMatchPolicy(policy string) (string, error) {
	if policy == "" {
		return TemplateMatchExact, nil
	}
	for _, p := range TemplateMatchPolicies {
		if p == policy {
			return p, nil
		}
	}
	return "", fmt.Errorf("template match policy %q is not one of %s", policy, strings.Join(TemplateMatchPolicies, ", "))
}

// ValidateTemplateTags checks that tags is non-empty and that every tag is a
// valid Proxmox tag.
func ValidateTemplateTags(tags []string) error {
	if len(tags) == 0 {
		return errors.New("at least one template tag is required")
	}
	for _, t := range tags {
		if !TemplateTagPattern.MatchString(t) {
			return fmt.Errorf("template tag %q is not a valid tag (letters, digits, _ . + -)", t)
		}
	}
	return nil
}

// SetMachineTemplateSelector makes every infrastructure MachineTemplate select
// its VM template by tag: it sets spec.template.spec.templateSelector.matchTags
// to tags, sets spec.template.spec.templateMatchPolicy to policy (and removes
// it for the default, exact), removes the fixed templateID and sourceNode the
// selector replaces, and re-renders b.YAML. The infrastructure provider then
// looks the template up on whichever node it schedules each machine to, so a
// template carrying the tags must exist on every allowed node.
func SetMachineTemplateSelector(b *Bundle, tags []string, policy string) error {
	if err := ValidateTemplateTags(tags); err != nil {
		return err
	}
	policy, err := NormalizeTemplateMatchPolicy(policy)
	if err != nil {
		return err
	}
	templates := Find(b, InfrastructureGroup, "MachineTemplate")
	if len(templates) == 0 {
		return fmt.Errorf("no %s MachineTemplate objects", InfrastructureGroup)
	}
	for _, obj := range templates {
		spec, _, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec")
		if err != nil {
			return errors.Wrapf(err, "reading spec of %s %q", obj.GetKind(), obj.GetName())
		}
		if spec == nil {
			spec = map[string]any{}
		}
		delete(spec, "templateID")
		delete(spec, "sourceNode")
		spec["templateSelector"] = map[string]any{"matchTags": toAnySlice(tags)}
		if policy == TemplateMatchExact {
			delete(spec, "templateMatchPolicy")
		} else {
			spec["templateMatchPolicy"] = policy
		}
		if err := unstructured.SetNestedMap(obj.Object, spec, "spec", "template", "spec"); err != nil {
			return errors.Wrapf(err, "setting template selector of %s %q", obj.GetKind(), obj.GetName())
		}
	}
	_, err = b.Render()
	return err
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
