// Package k8ssecret implements a backend.Store that keeps each cluster's state
// record in a Kubernetes Secret. One Secret, named klotho-state-<cluster>,
// holds the opaque, already-sealed record under a single data key in a
// configurable namespace of a target cluster.
//
// The store holds opaque, already-sealed bytes produced by the caller's state
// codec. It never sees plaintext state and does no encryption of its own; the
// Secret's value is the same ciphertext the file and s3 backends write.
//
// IMPORTANT: this backend needs a separate, already-running cluster that the
// klotho CLI can reach (its kubeconfig and context come from configuration). It
// cannot be the cluster klotho is bootstrapping: that cluster does not exist
// yet when its own state must first be written, so pointing this backend at it
// is a chicken-and-egg deadlock. Use a standing management or utility cluster.
// See the package README.
//
// Reserve is atomic: it Creates the Secret, and the API server itself rejects a
// second Create of the same name, so exactly one of several concurrent callers
// wins the name. Save is compare-and-swap: it reads the Secret's
// resourceVersion and Updates against it, retrying on a conflict, so two
// concurrent Saves never silently lose a write.
package k8ssecret

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// Constants shaping the Secret layout.
const (
	// SecretPrefix prefixes every state Secret's name, so Secrets this backend
	// owns are recognisable and List can recover a cluster name from one.
	SecretPrefix = "klotho-state-"
	// DataKey is the single data key under which the opaque, sealed record is
	// stored.
	DataKey = "record"
	// DefaultNamespace holds the Secrets when none is configured.
	DefaultNamespace = "default"

	// ManagedByLabel and ManagedByValue mark a Secret as klotho's, so a Secret
	// a person created by hand is never mistaken for cluster state.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "klotho"
	// ComponentLabel and ComponentValue mark a Secret as holding cluster state.
	ComponentLabel = "app.kubernetes.io/component"
	ComponentValue = "cluster-state"
	// ClusterLabel records the cluster name, so List reports it without having
	// to strip and re-validate the Secret name.
	ClusterLabel = "klotho.sh/cluster"

	// maxConflictRetries bounds how many times Save re-reads and re-applies
	// after an optimistic-concurrency conflict before giving up.
	maxConflictRetries = 5
)

var _ backend.Store = (*Store)(nil)

// namePattern is stricter than the shared cluster-name shape: a Secret name is
// a DNS subdomain, so a cluster name here must be lowercase and free of
// underscores. It must also start and end alphanumeric and be at most 63 bytes
// so that klotho-state-<name> is a valid Secret name and <name> is a valid
// label value. ValidateName additionally checks the composed Secret name.
var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)

// Sealer turns a state into an opaque record. The store uses it only to write
// reservation records, so it never handles keys itself.
type Sealer interface {
	Seal(*cluster.State) ([]byte, error)
}

// Config is the store's settings.
type Config struct {
	// Kubeconfig is the path to the kubeconfig of the target (management)
	// cluster. Empty uses the standard loading rules (KUBECONFIG, then
	// ~/.kube/config).
	Kubeconfig string
	// Context selects a context within the kubeconfig. Empty uses its
	// current-context.
	Context string
	// Namespace holds the Secrets. Empty means DefaultNamespace.
	Namespace string
}

// Option adjusts how New builds the store.
type Option func(*options)

type options struct {
	client kubernetes.Interface
}

// WithClient makes the store use client instead of building one from the
// kubeconfig. Tests pass a fake clientset; New still validates the
// configuration.
func WithClient(client kubernetes.Interface) Option {
	return func(o *options) { o.client = client }
}

// Store is a backend.Store backed by Kubernetes Secrets in one namespace.
type Store struct {
	secrets   corev1client.SecretInterface
	sealer    Sealer
	namespace string
}

// New builds a store for cfg, sealing reservation records with sealer. When no
// client is injected it builds a clientset from the kubeconfig; that does not
// touch the network, which is first reached on an actual operation.
func New(_ context.Context, cfg Config, sealer Sealer, opts ...Option) (*Store, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if sealer == nil {
		return nil, errors.New("kubernetes backend: sealer is required")
	}
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return nil, fmt.Errorf("kubernetes backend: namespace %q: %s", namespace, strings.Join(errs, "; "))
	}

	client := o.client
	if client == nil {
		var err error
		if client, err = newClient(cfg); err != nil {
			return nil, err
		}
	}
	return &Store{
		secrets:   client.CoreV1().Secrets(namespace),
		sealer:    sealer,
		namespace: namespace,
	}, nil
}

// Name returns "kubernetes".
func (s *Store) Name() string { return "kubernetes" }

// Namespace returns the namespace holding the Secrets.
func (s *Store) Namespace() string { return s.namespace }

// Reserve claims name by Creating its Secret. The API server rejects a second
// Create of the same name, so Reserve returns false without error when the
// name is already taken. No read-then-write is involved, so there is no race
// between concurrent callers.
func (s *Store) Reserve(ctx context.Context, name string) (bool, error) {
	if err := ValidateName(name); err != nil {
		return false, err
	}
	res := state.NewReservation()
	res.Spec.Name = name
	record, err := s.sealer.Seal(res)
	if err != nil {
		return false, fmt.Errorf("kubernetes backend: seal reservation for %s: %w", name, err)
	}
	if _, err := s.secrets.Create(ctx, s.secret(name, record), metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("kubernetes backend: reserve %s: %w", name, err)
	}
	return true, nil
}

// Load returns the record for name, or backend.ErrNotFound.
func (s *Store) Load(ctx context.Context, name string) ([]byte, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	sec, err := s.secrets.Get(ctx, secretName(name), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("kubernetes backend: %s: %w", name, backend.ErrNotFound)
		}
		return nil, fmt.Errorf("kubernetes backend: load %s: %w", name, err)
	}
	return sec.Data[DataKey], nil
}

// Save writes the record for name, creating the Secret when it does not yet
// exist and otherwise replacing its contents with an optimistic-concurrency
// Update: it carries the observed resourceVersion, so the API server rejects
// the write when another caller changed the Secret first, and Save re-reads and
// retries. This is the compare-and-swap the file and s3 backends lack.
func (s *Store) Save(ctx context.Context, name string, record []byte) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		cur, err := s.secrets.Get(ctx, secretName(name), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if _, err := s.secrets.Create(ctx, s.secret(name, record), metav1.CreateOptions{}); err != nil {
				if apierrors.IsAlreadyExists(err) && attempt < maxConflictRetries {
					continue // another caller created it; fall through to Update
				}
				return fmt.Errorf("kubernetes backend: save %s: %w", name, err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("kubernetes backend: save %s: %w", name, err)
		}
		desired := s.secret(name, record)
		desired.ResourceVersion = cur.ResourceVersion
		if _, err := s.secrets.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) && attempt < maxConflictRetries {
				continue
			}
			return fmt.Errorf("kubernetes backend: save %s: %w", name, err)
		}
		return nil
	}
}

// Delete removes the record for name, or returns backend.ErrNotFound.
func (s *Store) Delete(ctx context.Context, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := s.secrets.Delete(ctx, secretName(name), metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("kubernetes backend: %s: %w", name, backend.ErrNotFound)
		}
		return fmt.Errorf("kubernetes backend: delete %s: %w", name, err)
	}
	return nil
}

// List returns the names of every cluster the backend knows, sorted. It selects
// only the Secrets this backend labels, so a Secret a person put in the
// namespace by hand is never mistaken for a cluster. The cluster name comes
// from the ClusterLabel, falling back to the Secret name; a Secret whose
// recovered name is not valid is ignored.
func (s *Store) List(ctx context.Context) ([]string, error) {
	selector := labels.SelectorFromSet(labels.Set{
		ManagedByLabel: ManagedByValue,
		ComponentLabel: ComponentValue,
	}).String()
	l, err := s.secrets.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("kubernetes backend: list namespace %s: %w", s.namespace, err)
	}
	names := make([]string, 0, len(l.Items))
	for i := range l.Items {
		name := l.Items[i].Labels[ClusterLabel]
		if name == "" {
			var ok bool
			if name, ok = strings.CutPrefix(l.Items[i].Name, SecretPrefix); !ok {
				continue
			}
		}
		if ValidateName(name) != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// ValidateName reports whether name is usable as a cluster name in this
// backend. The name must be lowercase (no uppercase or underscores, unlike the
// other backends), so that klotho-state-<name> is a valid Secret name and
// <name> is a valid label value.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("kubernetes backend: invalid cluster name %q: must match %s (a lowercase DNS name: letters, digits, '-' and '.', starting and ending alphanumeric)", name, namePattern)
	}
	if errs := validation.IsDNS1123Subdomain(secretName(name)); len(errs) > 0 {
		return fmt.Errorf("kubernetes backend: invalid cluster name %q: %s", name, strings.Join(errs, "; "))
	}
	return nil
}

// secretName is the Secret name for a cluster.
func secretName(name string) string { return SecretPrefix + name }

// secret builds the Secret object for a cluster's record.
func (s *Store) secret(name string, record []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName(name),
			Namespace: s.namespace,
			Labels: map[string]string{
				ManagedByLabel: ManagedByValue,
				ComponentLabel: ComponentValue,
				ClusterLabel:   name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{DataKey: record},
	}
}
