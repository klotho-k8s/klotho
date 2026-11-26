// Package file implements a backend.Store on a local directory, one file per
// cluster at <dir>/<name>.record.
//
// Reserve is atomic on a local filesystem: it creates the record with
// O_CREATE|O_EXCL, so exactly one of several concurrent callers wins. Save
// replaces a record atomically by writing a temporary file in the same
// directory and renaming it over the old one. Network filesystems may weaken
// either guarantee.
package file

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/klotho-k8s/klotho/pkg/cluster"
	"github.com/klotho-k8s/klotho/pkg/state"
	"github.com/klotho-k8s/klotho/provider/backend"
)

// Suffix is the file extension of a cluster record.
const Suffix = ".record"

var _ backend.Store = (*Store)(nil)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Sealer turns a state into an opaque record. The store uses it only to write
// reservation records, so it never handles keys itself.
type Sealer interface {
	Seal(*cluster.State) ([]byte, error)
}

// Store is a backend.Store on a local directory.
type Store struct {
	dir    string
	sealer Sealer
}

// New returns a store rooted at dir, creating the directory with mode 0700 if
// it does not exist.
func New(dir string, sealer Sealer) (*Store, error) {
	if dir == "" {
		return nil, errors.New("file backend: directory is required")
	}
	if sealer == nil {
		return nil, errors.New("file backend: sealer is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("file backend: create %s: %w", dir, err)
	}
	return &Store{dir: dir, sealer: sealer}, nil
}

// Name returns "file".
func (s *Store) Name() string { return "file" }

// Dir returns the directory holding the records.
func (s *Store) Dir() string { return s.dir }

// Reserve claims name by exclusively creating its record file. It returns
// false if the record already exists.
func (s *Store) Reserve(ctx context.Context, name string) (bool, error) {
	path, err := s.path(name)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	res := state.NewReservation()
	res.Spec.Name = name
	record, err := s.sealer.Seal(res)
	if err != nil {
		return false, fmt.Errorf("file backend: seal reservation for %s: %w", name, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("file backend: reserve %s: %w", name, err)
	}
	if err := writeAndClose(f, record); err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("file backend: reserve %s: %w", name, err)
	}
	return true, nil
}

// Load returns the record for name, or backend.ErrNotFound.
func (s *Store) Load(ctx context.Context, name string) ([]byte, error) {
	path, err := s.path(name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("file backend: %s: %w", name, backend.ErrNotFound)
		}
		return nil, fmt.Errorf("file backend: load %s: %w", name, err)
	}
	return b, nil
}

// Save atomically replaces the record for name.
func (s *Store) Save(ctx context.Context, name string, record []byte) error {
	path, err := s.path(name)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// CreateTemp creates the file with mode 0600. Its name never ends in
	// Suffix, so List ignores it.
	tmp, err := os.CreateTemp(s.dir, "."+name+".*.tmp")
	if err != nil {
		return fmt.Errorf("file backend: save %s: %w", name, err)
	}
	tmpPath := tmp.Name()
	if err := writeAndClose(tmp, record); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("file backend: save %s: %w", name, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("file backend: save %s: %w", name, err)
	}
	return nil
}

// Delete removes the record for name, or returns backend.ErrNotFound.
func (s *Store) Delete(ctx context.Context, name string) error {
	path, err := s.path(name)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("file backend: %s: %w", name, backend.ErrNotFound)
		}
		return fmt.Errorf("file backend: delete %s: %w", name, err)
	}
	return nil
}

// List returns the names of all records, sorted. Files without the record
// suffix, directories, and files whose names are not valid cluster names are
// ignored.
func (s *Store) List(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("file backend: list %s: %w", s.dir, err)
	}
	names := []string{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name, ok := strings.CutSuffix(e.Name(), Suffix)
		if !ok || ValidateName(name) != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// ValidateName reports whether name is usable as a cluster name in this
// backend. Names must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$ and must not be
// a reserved Windows device name, so a store directory stays portable.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("file backend: invalid cluster name %q: must match %s", name, namePattern)
	}
	base, _, _ := strings.Cut(name, ".")
	if isWindowsDeviceName(base) {
		return fmt.Errorf("file backend: invalid cluster name %q: reserved device name", name)
	}
	return nil
}

func (s *Store) path(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, name+Suffix), nil
}

func isWindowsDeviceName(base string) bool {
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	u := strings.ToUpper(base)
	if len(u) == 4 && (strings.HasPrefix(u, "COM") || strings.HasPrefix(u, "LPT")) {
		return u[3] >= '0' && u[3] <= '9'
	}
	return false
}

func writeAndClose(f *os.File, b []byte) error {
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
