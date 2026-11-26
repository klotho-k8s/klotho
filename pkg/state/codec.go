package state

import (
	"bytes"
	"errors"

	"filippo.io/age"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

// RecordCodec converts between a state and a single-line record.
type RecordCodec interface {
	Seal(s *cluster.State) ([]byte, error)
	Open(record []byte) (*cluster.State, error)
}

var (
	_ RecordCodec = (*Codec)(nil)
	_ RecordCodec = PlainCodec{}
)

// Codec seals states as encrypted records and opens them again. A codec
// used only to seal needs no identities; one used only to open needs no
// recipients.
type Codec struct {
	recipients []age.Recipient
	identities []age.Identity
}

// NewCodec returns a codec that encrypts to recipients and decrypts with
// identities. At least one of the two must be non-empty.
func NewCodec(recipients []age.Recipient, identities []age.Identity) (*Codec, error) {
	if len(recipients) == 0 && len(identities) == 0 {
		return nil, errors.New("state: codec needs recipients or identities")
	}
	return &Codec{
		recipients: append([]age.Recipient(nil), recipients...),
		identities: append([]age.Identity(nil), identities...),
	}, nil
}

// Seal encrypts a state and encodes it as a single-line record.
func (c *Codec) Seal(s *cluster.State) ([]byte, error) {
	armored, err := Encrypt(s, c.recipients)
	if err != nil {
		return nil, err
	}
	return EncodeRecord(armored), nil
}

// Open decodes and decrypts a record. It refuses plain records.
func (c *Codec) Open(record []byte) (*cluster.State, error) {
	if IsPlainRecord(record) {
		return nil, errors.New("state: record is unencrypted; it can only be opened with a plain codec")
	}
	armored, err := DecodeRecord(record)
	if err != nil {
		return nil, err
	}
	return Decrypt(armored, c.identities)
}

// PlainCodec stores the JSON document unencrypted. It is for local
// development only: the record holds the cluster CA key in the clear.
type PlainCodec struct{}

// Seal encodes a state as a plain single-line record.
func (PlainCodec) Seal(s *cluster.State) ([]byte, error) {
	doc, err := Marshal(s)
	if err != nil {
		return nil, err
	}
	// encoding/json escapes control characters, so doc is a single line.
	return append([]byte(PlainMarker), doc...), nil
}

// Open decodes a plain record. It refuses encrypted records.
func (PlainCodec) Open(record []byte) (*cluster.State, error) {
	record = bytes.TrimSpace(record)
	if bytes.HasPrefix(record, []byte(Marker)) {
		return nil, errors.New("state: record is encrypted; a plain codec cannot open it")
	}
	if !bytes.HasPrefix(record, []byte(PlainMarker)) {
		return nil, errors.New("state: not a klotho record")
	}
	return Unmarshal(record[len(PlainMarker):])
}
