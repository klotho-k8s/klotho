// Package proxmox implements a backend.Store that keeps each cluster's state
// record in the comment of a dedicated Proxmox VE resource pool. Every
// cluster gets one pool, named <prefix><cluster> (prefix "klotho-" by
// default). The pool normally has no members.
//
// Records are opaque single lines produced by the caller's sealer and always
// start with a record marker (state.Marker or state.PlainMarker). A pool
// whose comment does not carry a marker is never treated as a cluster record:
// Load and Delete refuse it with ErrNotAKlothoPool, and List skips it.
//
// Reserve is atomic across clients of the same Proxmox cluster: it creates
// the pool, and Proxmox rejects a second pool with the same ID, so exactly
// one of several concurrent callers wins. Save replaces the comment in one
// API call; there is no compare-and-swap, so concurrent Saves are
// last-writer-wins.
package proxmox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	gp "github.com/luthermonson/go-proxmox"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// DefaultPrefix is the pool ID prefix used when WithPrefix is not given.
const DefaultPrefix = "klotho-"

var _ backend.Store = (*Store)(nil)

// ErrNotAKlothoPool is returned when a pool with the cluster's ID exists but
// its comment is not a cluster record, for example a pool a person created by
// hand. The store never returns such a comment as a record, never deletes
// such a pool, and never lists it as a cluster.
var ErrNotAKlothoPool = errors.New("pool exists but does not hold a klotho record")

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Sealer turns a state into an opaque record. The store uses it only to write
// reservation records, so it never handles keys itself.
type Sealer interface {
	Seal(*cluster.State) ([]byte, error)
}

// Option configures a Store.
type Option func(*Store)

// WithPrefix sets the pool ID prefix. The prefix plus a cluster name must
// form a valid Proxmox pool ID.
func WithPrefix(prefix string) Option {
	return func(s *Store) { s.prefix = prefix }
}

// Store is a backend.Store on Proxmox resource pool comments.
type Store struct {
	client *gp.Client
	sealer Sealer
	prefix string
}

// New returns a store that talks to Proxmox through client and seals
// reservation records with sealer.
func New(client *gp.Client, sealer Sealer, opts ...Option) *Store {
	s := &Store{client: client, sealer: sealer, prefix: DefaultPrefix}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Name returns "proxmox".
func (s *Store) Name() string { return "proxmox" }

// Prefix returns the pool ID prefix.
func (s *Store) Prefix() string { return s.prefix }

// PoolID returns the ID of the pool that holds the named cluster's record.
// It does not validate the name.
func (s *Store) PoolID(name string) string { return s.prefix + name }

// ValidateName reports whether name is usable as a cluster name: it must
// match ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("proxmox backend: invalid cluster name %q: must match %s", name, namePattern)
	}
	return nil
}

func (s *Store) poolID(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	id := s.PoolID(name)
	if err := validatePoolID(id); err != nil {
		return "", fmt.Errorf("proxmox backend: cluster %s: %w", name, err)
	}
	return id, nil
}

// Reserve claims name by creating its pool with a sealed reservation record
// as the comment. It returns false if the pool already exists, whoever
// created it.
func (s *Store) Reserve(ctx context.Context, name string) (bool, error) {
	id, err := s.poolID(name)
	if err != nil {
		return false, err
	}
	if s.sealer == nil {
		return false, errors.New("proxmox backend: sealer is required")
	}
	res := state.NewReservation()
	res.Spec.Name = name
	record, err := s.sealer.Seal(res)
	if err != nil {
		return false, fmt.Errorf("proxmox backend: seal reservation for %s: %w", name, err)
	}
	if err := checkRecord(record); err != nil {
		return false, fmt.Errorf("proxmox backend: reservation for %s: %w", name, err)
	}
	if err := createPool(ctx, s.client, id, string(record)); err != nil {
		if errors.Is(err, errPoolExists) {
			return false, nil
		}
		return false, fmt.Errorf("proxmox backend: reserve %s: %w", name, err)
	}
	return true, nil
}

// Load returns the record for name, or backend.ErrNotFound. A pool that
// exists without a record yields ErrNotAKlothoPool.
func (s *Store) Load(ctx context.Context, name string) ([]byte, error) {
	p, err := s.ownedPool(ctx, "load", name)
	if err != nil {
		return nil, err
	}
	return []byte(p.Comment), nil
}

// Save replaces the record for name. The record must carry a record marker
// and fit on one line. It returns backend.ErrNotFound if the pool is gone.
func (s *Store) Save(ctx context.Context, name string, record []byte) error {
	id, err := s.poolID(name)
	if err != nil {
		return err
	}
	if err := checkRecord(record); err != nil {
		return fmt.Errorf("proxmox backend: save %s: %w", name, err)
	}
	if err := setComment(ctx, s.client, id, string(record)); err != nil {
		return wrap("save", name, err)
	}
	return nil
}

// Delete removes the cluster's pool. Guest members (qemu and lxc) are first
// removed from the pool, which leaves the guests themselves untouched.
// Storage members cannot be removed through the guest member list, so a pool
// holding any is refused, before anything is changed, with an error naming
// them. A pool that is not a klotho pool is refused with ErrNotAKlothoPool.
func (s *Store) Delete(ctx context.Context, name string) error {
	p, err := s.ownedPool(ctx, "delete", name)
	if err != nil {
		return err
	}
	var (
		vmids []string
		other []string
	)
	for _, m := range p.Members {
		switch m.Type {
		case "qemu", "lxc":
			vmids = append(vmids, strconv.FormatUint(m.VMID, 10))
		default:
			other = append(other, m.ID)
		}
	}
	if len(other) > 0 {
		sort.Strings(other)
		return fmt.Errorf("proxmox backend: delete %s: pool %s has members that must be removed by hand first: %s",
			name, p.PoolID, strings.Join(other, ", "))
	}
	if len(vmids) > 0 {
		opt := &gp.PoolUpdateOption{VirtualMachines: strings.Join(vmids, ","), Delete: true}
		if err := classifyNotFound(p.Update(ctx, opt)); err != nil {
			return wrap("delete", name, err)
		}
	}
	if err := classifyNotFound(p.Delete(ctx)); err != nil {
		return wrap("delete", name, err)
	}
	return nil
}

// List returns the names of all clusters, sorted. Only pools whose ID starts
// with the prefix and whose comment carries a record marker are included;
// pools with the prefix but some other comment are skipped silently, since
// they belong to someone else.
func (s *Store) List(ctx context.Context) ([]string, error) {
	pools, err := s.client.Pools(ctx)
	if err != nil {
		return nil, fmt.Errorf("proxmox backend: list: %w", err)
	}
	names := []string{}
	for _, p := range pools {
		if p == nil {
			continue
		}
		name, ok := strings.CutPrefix(p.PoolID, s.prefix)
		if !ok || ValidateName(name) != nil || !state.IsRecord([]byte(p.Comment)) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// ownedPool fetches the cluster's pool and checks that it holds a record.
func (s *Store) ownedPool(ctx context.Context, op, name string) (*gp.Pool, error) {
	id, err := s.poolID(name)
	if err != nil {
		return nil, err
	}
	p, err := getPool(ctx, s.client, id)
	if err != nil {
		return nil, wrap(op, name, err)
	}
	if p == nil || !state.IsRecord([]byte(p.Comment)) {
		return nil, fmt.Errorf("proxmox backend: %s %s: pool %s: %w", op, name, id, ErrNotAKlothoPool)
	}
	return p, nil
}

func wrap(op, name string, err error) error {
	if errors.Is(err, errPoolNotFound) {
		return fmt.Errorf("proxmox backend: %s: %w", name, backend.ErrNotFound)
	}
	return fmt.Errorf("proxmox backend: %s %s: %w", op, name, err)
}

// checkRecord rejects anything that is not a single-line record.
func checkRecord(record []byte) error {
	if !state.IsRecord(record) {
		return errors.New("not a klotho record")
	}
	if bytes.ContainsAny(record, "\r\n") {
		return errors.New("record contains a line break")
	}
	return nil
}
