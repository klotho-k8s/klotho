package proxmox

import (
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/generate"
)

// Resolved is the provider configuration after applying the precedence
// Config field > spec variable > default. Values that can only be decided
// against the live cluster (the node when nothing names one, the ISO storage
// when none is configured) are left empty here and chosen by Prepare and
// Launch.
type Resolved struct {
	Cluster string
	// Node is empty when it is to be chosen at run time.
	Node string
	// TemplateID is zero when TemplateTag or TemplateTags selects the
	// template. With TemplateTags it may be set too, and then names the
	// template the tags must select on the bootstrap node.
	TemplateID  int
	TemplateTag string
	// TemplateTags and TemplateMatchPolicy select the template by tags on
	// the bootstrap node; see Config.
	TemplateTags        []string
	TemplateMatchPolicy string
	// ISOStorage is empty when it is to be chosen at run time.
	ISOStorage string
	Bridge     string
	// BridgeSet reports whether Bridge came from Config or BRIDGE, in which
	// case net0 is rewritten; otherwise the cloned net0 is kept.
	BridgeSet bool
	// Cores, Sockets and MemoryMiB are zero when the template's values stay.
	Cores     int
	Sockets   int
	MemoryMiB int
	// BootDevice and BootSizeGiB grow the cloned boot disk before the first
	// boot, from the same template variables the machine templates use, so
	// the bootstrap node matches the nodes the control plane provider will
	// make. Zero leaves the template's disk as it is.
	BootDevice   string
	BootSizeGiB  int
	VMIDRange    [2]int
	FullClone    bool
	Pool         string
	AgentTimeout time.Duration
	Tag          string
	VMName       string
	ISOName      string
}

// Resolve applies spec's variables and the defaults to the provider's
// configuration. It makes no API calls.
func (p *Provider) Resolve(spec *cluster.Spec) (Resolved, error) {
	if spec == nil || spec.Name == "" {
		return Resolved{}, errorf("a cluster name is required")
	}
	c := p.cfg
	vars := spec.Variables
	r := Resolved{
		Cluster:      spec.Name,
		Tag:          Tag(spec.Name),
		VMName:       VMName(spec.Name),
		ISOName:      ISOName(spec.Name),
		TemplateTag:  c.TemplateTag,
		ISOStorage:   c.ISOStorage,
		Pool:         c.Pool,
		FullClone:    c.FullClone == nil || *c.FullClone,
		AgentTimeout: c.AgentTimeout,
		VMIDRange:    c.VMIDRange,
	}
	if !tagPattern.MatchString(r.Tag) {
		return Resolved{}, errorf("cluster name %q does not make a valid Proxmox tag %q", spec.Name, r.Tag)
	}
	if r.AgentTimeout <= 0 {
		r.AgentTimeout = DefaultAgentTimeout
	}
	if c.Pool != "" {
		if err := validatePoolID(c.Pool); err != nil {
			return Resolved{}, err
		}
	}
	if lo, hi := r.VMIDRange[0], r.VMIDRange[1]; lo != 0 || hi != 0 {
		if lo <= 0 || hi < lo {
			return Resolved{}, errorf("invalid VMID range [%d, %d]", lo, hi)
		}
	}

	// Node.
	switch {
	case c.Node != "":
		r.Node = c.Node
	default:
		allowed, err := parseList(vars[VarAllowedNodes])
		if err != nil {
			return Resolved{}, errorf("%s: %v", VarAllowedNodes, err)
		}
		if len(allowed) > 0 {
			r.Node = allowed[0]
		} else {
			r.Node = strings.TrimSpace(vars[VarSourceNode])
		}
	}

	// Template.
	if len(c.TemplateTags) > 0 {
		if err := generate.ValidateTemplateTags(c.TemplateTags); err != nil {
			return Resolved{}, errorf("template tags: %v", err)
		}
		policy, err := generate.NormalizeTemplateMatchPolicy(c.TemplateMatchPolicy)
		if err != nil {
			return Resolved{}, errorf("%v", err)
		}
		r.TemplateTags = append([]string(nil), c.TemplateTags...)
		r.TemplateMatchPolicy = policy
		r.TemplateTag = ""
	}
	switch {
	case c.TemplateID > 0:
		r.TemplateID = c.TemplateID
		r.TemplateTag = ""
	case len(c.TemplateTags) > 0:
	case c.TemplateTag != "":
	default:
		id, ok, err := intVar(vars, VarTemplateID)
		if err != nil {
			return Resolved{}, err
		}
		if !ok || id <= 0 {
			return Resolved{}, errorf("no template: set TemplateID or TemplateTag, or the %s variable", VarTemplateID)
		}
		r.TemplateID = id
	}

	// Bridge.
	switch {
	case c.Bridge != "":
		r.Bridge, r.BridgeSet = c.Bridge, true
	case strings.TrimSpace(vars[VarBridge]) != "":
		r.Bridge, r.BridgeSet = strings.TrimSpace(vars[VarBridge]), true
	default:
		r.Bridge = DefaultBridge
	}

	// Sizing.
	var err error
	if r.Cores, err = pick(c.Cores, vars, VarNumCores); err != nil {
		return Resolved{}, err
	}
	if r.Sockets, err = pick(c.Sockets, vars, VarNumSockets); err != nil {
		return Resolved{}, err
	}
	if r.MemoryMiB, err = pick(c.MemoryMiB, vars, VarMemoryMiB); err != nil {
		return Resolved{}, err
	}
	if r.BootSizeGiB, err = pick(0, vars, VarBootVolumeSize); err != nil {
		return Resolved{}, err
	}
	r.BootDevice = strings.TrimSpace(vars[VarBootVolumeDevice])
	if r.BootSizeGiB > 0 && r.BootDevice == "" {
		r.BootDevice = DefaultBootDevice
	}
	return r, nil
}

func pick(field int, vars map[string]string, name string) (int, error) {
	if field > 0 {
		return field, nil
	}
	v, ok, err := intVar(vars, name)
	if err != nil || !ok {
		return 0, err
	}
	if v < 0 {
		return 0, errorf("%s must not be negative, got %d", name, v)
	}
	return v, nil
}

func intVar(vars map[string]string, name string) (int, bool, error) {
	s := strings.TrimSpace(vars[name])
	if s == "" {
		return 0, false, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false, errorf("%s=%q is not an integer", name, s)
	}
	return v, true, nil
}

// parseList reads a list variable such as ALLOWED_NODES, which templates take
// as a YAML flow sequence ("[pve1, pve2]") but people also write as a plain
// comma-separated list.
func parseList(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if strings.HasPrefix(s, "[") {
		var out []string
		if err := yaml.Unmarshal([]byte(s), &out); err != nil {
			return nil, err
		}
		return compact(out), nil
	}
	return compact(strings.Split(s, ",")), nil
}

func compact(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
			out = append(out, v)
		}
	}
	return out
}
