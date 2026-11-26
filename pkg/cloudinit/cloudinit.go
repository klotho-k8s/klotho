// Package cloudinit turns provider contributions into a cloud-init NoCloud
// datasource: a #cloud-config user-data document, meta-data, and an optional
// network-config, packed into an ISO9660 image labelled "cidata" or written to
// a directory for inspection.
package cloudinit

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"sigs.k8s.io/yaml"

	"github.com/klotho-k8s/klotho/provider/controlplane"
	"github.com/klotho-k8s/klotho/provider/payload"
)

// Header is the first line every cloud-config user-data document must carry.
const Header = "#cloud-config"

const (
	defaultMode  = "0644"
	defaultOwner = "root:root"
)

// Bundle is a rendered NoCloud datasource.
type Bundle struct {
	UserData []byte
	MetaData []byte
	// NetworkConfig is nil when no network configuration was requested.
	NetworkConfig []byte
}

// Options are the node-level settings that do not come from a provider.
type Options struct {
	Hostname   string
	InstanceID string
	// SSHAuthorizedKeys are installed for the image's default user.
	SSHAuthorizedKeys []string
	Network           *NetworkConfig
	PackageUpdate     bool
	// Fetches replace files that were published to a payload host. Their
	// commands run before anything else, and any contributed file at the same
	// path is dropped from write_files.
	Fetches  []payload.Fetch
	Timezone string
}

type cloudConfig struct {
	Hostname          string      `json:"hostname,omitempty"`
	FQDN              string      `json:"fqdn,omitempty"`
	PreserveHostname  *bool       `json:"preserve_hostname,omitempty"`
	Timezone          string      `json:"timezone,omitempty"`
	PackageUpdate     bool        `json:"package_update,omitempty"`
	SSHAuthorizedKeys []string    `json:"ssh_authorized_keys,omitempty"`
	WriteFiles        []writeFile `json:"write_files,omitempty"`
	RunCmd            []string    `json:"runcmd,omitempty"`
}

type writeFile struct {
	Path        string `json:"path"`
	Content     string `json:"content"`
	Encoding    string `json:"encoding,omitempty"`
	Owner       string `json:"owner"`
	Permissions string `json:"permissions"`
	Append      bool   `json:"append,omitempty"`
}

type metaData struct {
	InstanceID    string `json:"instance-id"`
	LocalHostname string `json:"local-hostname,omitempty"`
}

// Build merges the contributions, in order, with opts into a NoCloud bundle.
//
// Files become write_files entries. A path may appear more than once only if
// every later occurrence appends; any other repeat is an error naming the
// contribution indexes involved. runcmd is the fetch commands, then every
// contribution's PreCommands, then every InstallCommands, then every
// PostCommands.
func Build(contribs []controlplane.Contribution, opts Options) (*Bundle, error) {
	instanceID := opts.InstanceID
	if instanceID == "" {
		if opts.Hostname == "" {
			return nil, errors.New("cloudinit: a hostname or an instance ID is required")
		}
		instanceID = DeriveInstanceID(opts.Hostname)
	}

	fetched := make(map[string]bool, len(opts.Fetches))
	var runcmd []string
	for i, f := range opts.Fetches {
		if f.Path == "" || f.Command == "" {
			return nil, fmt.Errorf("cloudinit: fetch %d needs both a path and a command", i)
		}
		fetched[path.Clean(f.Path)] = true
		runcmd = append(runcmd, f.Command)
	}

	files, err := mergeFiles(contribs, fetched)
	if err != nil {
		return nil, err
	}
	for _, c := range contribs {
		runcmd = append(runcmd, c.PreCommands...)
	}
	for _, c := range contribs {
		runcmd = append(runcmd, c.InstallCommands...)
	}
	for _, c := range contribs {
		runcmd = append(runcmd, c.PostCommands...)
	}

	cfg := cloudConfig{
		Timezone:          opts.Timezone,
		PackageUpdate:     opts.PackageUpdate,
		SSHAuthorizedKeys: opts.SSHAuthorizedKeys,
		WriteFiles:        files,
		RunCmd:            runcmd,
	}
	if opts.Hostname != "" {
		short, _, isFQDN := strings.Cut(opts.Hostname, ".")
		cfg.Hostname = short
		if isFQDN {
			cfg.FQDN = opts.Hostname
		}
		preserve := false
		cfg.PreserveHostname = &preserve
	}

	body, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("cloudinit: marshalling user-data: %w", err)
	}
	userData := make([]byte, 0, len(Header)+1+len(body))
	userData = append(userData, Header+"\n"...)
	userData = append(userData, body...)
	if _, err := Parse(userData); err != nil {
		return nil, fmt.Errorf("cloudinit: rendered user-data does not parse back: %w", err)
	}

	md, err := yaml.Marshal(metaData{InstanceID: instanceID, LocalHostname: opts.Hostname})
	if err != nil {
		return nil, fmt.Errorf("cloudinit: marshalling meta-data: %w", err)
	}

	b := &Bundle{UserData: userData, MetaData: md}
	if opts.Network != nil {
		nc, err := opts.Network.Marshal()
		if err != nil {
			return nil, err
		}
		b.NetworkConfig = nc
	}
	return b, nil
}

func mergeFiles(contribs []controlplane.Contribution, fetched map[string]bool) ([]writeFile, error) {
	owners := map[string]int{}
	var out []writeFile
	for ci, c := range contribs {
		for fi, f := range c.Files {
			if f.Path == "" {
				return nil, fmt.Errorf("cloudinit: contribution %d file %d has no path", ci, fi)
			}
			if !strings.HasPrefix(f.Path, "/") {
				return nil, fmt.Errorf("cloudinit: contribution %d file %q: path must be absolute", ci, f.Path)
			}
			p := path.Clean(f.Path)
			first, seen := owners[p]
			if seen && !f.Append {
				return nil, fmt.Errorf("cloudinit: duplicate write_files path %q in contributions %d and %d", p, first, ci)
			}
			if !seen {
				owners[p] = ci
			}
			if fetched[p] {
				continue
			}
			out = append(out, toWriteFile(p, f))
		}
	}
	return out, nil
}

func toWriteFile(p string, f controlplane.File) writeFile {
	wf := writeFile{
		Path:        p,
		Owner:       f.Owner,
		Permissions: f.Mode,
		Append:      f.Append,
	}
	if wf.Owner == "" {
		wf.Owner = defaultOwner
	}
	if wf.Permissions == "" {
		wf.Permissions = defaultMode
	}
	if f.Base64 || !embeddable(f.Content) {
		wf.Encoding = "b64"
		wf.Content = base64.StdEncoding.EncodeToString(f.Content)
	} else {
		wf.Content = string(f.Content)
	}
	return wf
}

// embeddable reports whether content survives as a plain YAML string: valid
// UTF-8 with no control or non-printable characters other than newline and
// tab. Carriage returns are excluded because YAML line-break normalisation
// would rewrite them; a byte-order mark is excluded because parsers may strip
// it.
func embeddable(content []byte) bool {
	if !utf8.Valid(content) {
		return false
	}
	for _, r := range string(content) {
		if r == '\n' || r == '\t' || r == ' ' {
			continue
		}
		if r == 0xFEFF || unicode.IsControl(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// DeriveInstanceID returns a stable instance ID for a hostname, so rebuilding
// the datasource for the same node does not make cloud-init treat it as a new
// instance.
func DeriveInstanceID(hostname string) string {
	sum := sha256.Sum256([]byte(hostname))
	return "iid-" + hex.EncodeToString(sum[:8])
}

// Parse strips the #cloud-config header from user-data and unmarshals the
// rest. It fails if the header is missing.
func Parse(userData []byte) (map[string]any, error) {
	first, rest, _ := bytes.Cut(userData, []byte("\n"))
	if string(bytes.TrimRight(first, " \t\r")) != Header {
		return nil, fmt.Errorf("cloudinit: user-data does not start with %q", Header)
	}
	out := map[string]any{}
	if err := yaml.Unmarshal(rest, &out); err != nil {
		return nil, fmt.Errorf("cloudinit: parsing user-data: %w", err)
	}
	return out, nil
}
