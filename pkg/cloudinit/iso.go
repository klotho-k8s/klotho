package cloudinit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kdomanski/iso9660"
)

// VolumeLabel is the volume identifier NoCloud looks for.
const VolumeLabel = "cidata"

// File names NoCloud reads from the root of the datasource.
const (
	UserDataFile      = "user-data"
	MetaDataFile      = "meta-data"
	NetworkConfigFile = "network-config"
)

func (b *Bundle) files() ([]string, [][]byte, error) {
	if b == nil || len(b.UserData) == 0 || len(b.MetaData) == 0 {
		return nil, nil, errors.New("cloudinit: bundle needs user-data and meta-data")
	}
	names := []string{UserDataFile, MetaDataFile}
	data := [][]byte{b.UserData, b.MetaData}
	if b.NetworkConfig != nil {
		names = append(names, NetworkConfigFile)
		data = append(data, b.NetworkConfig)
	}
	return names, data, nil
}

// ISO renders the bundle as an ISO9660 image labelled cidata.
func ISO(b *Bundle) ([]byte, error) {
	var buf bytes.Buffer
	if err := ISOTo(&buf, b); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ISOTo streams the bundle as an ISO9660 image labelled cidata to w.
func ISOTo(w io.Writer, b *Bundle) (err error) {
	names, data, err := b.files()
	if err != nil {
		return err
	}
	iw, err := iso9660.NewWriter()
	if err != nil {
		return fmt.Errorf("cloudinit: creating iso writer: %w", err)
	}
	defer func() {
		if cerr := iw.Cleanup(); cerr != nil && err == nil {
			err = fmt.Errorf("cloudinit: cleaning up iso staging: %w", cerr)
		}
	}()
	for i, name := range names {
		if err := iw.AddFile(bytes.NewReader(data[i]), name); err != nil {
			return fmt.Errorf("cloudinit: adding %s to iso: %w", name, err)
		}
	}
	if err := iw.WriteTo(w, VolumeLabel); err != nil {
		return fmt.Errorf("cloudinit: writing iso: %w", err)
	}
	return nil
}

// WriteDir writes the bundle's files into dir, creating it if needed, each
// with mode 0600. A network-config left over from an earlier run is removed
// when the bundle has none.
func WriteDir(dir string, b *Bundle) error {
	names, data, err := b.files()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cloudinit: %w", err)
	}
	for i, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), data[i], 0o600); err != nil {
			return fmt.Errorf("cloudinit: %w", err)
		}
	}
	if b.NetworkConfig == nil {
		if err := os.Remove(filepath.Join(dir, NetworkConfigFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cloudinit: %w", err)
		}
	}
	return nil
}
