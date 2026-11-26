package state

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

const (
	// Marker prefixes an encrypted record.
	Marker = "klotho/v1:"
	// PlainMarker prefixes an unencrypted record.
	PlainMarker = "klotho/v1-plain:"

	// maxPlaintext bounds how much decrypted data is read from one record.
	maxPlaintext = 64 << 20
)

// Encrypt encodes a state as a document and encrypts it to every recipient,
// returning ASCII-armored age ciphertext.
func Encrypt(s *cluster.State, recipients []age.Recipient) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errors.New("state: encrypt: no recipients")
	}
	doc, err := Marshal(s)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	aw := armor.NewWriter(&buf)
	w, err := age.Encrypt(aw, recipients...)
	if err != nil {
		return nil, fmt.Errorf("state: encrypt: %w", err)
	}
	if _, err := w.Write(doc); err != nil {
		return nil, fmt.Errorf("state: encrypt: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("state: encrypt: %w", err)
	}
	if err := aw.Close(); err != nil {
		return nil, fmt.Errorf("state: armor: %w", err)
	}
	return buf.Bytes(), nil
}

// Decrypt decrypts ASCII-armored age ciphertext with any of the identities
// and decodes the document inside it.
func Decrypt(armored []byte, identities []age.Identity) (*cluster.State, error) {
	if len(identities) == 0 {
		return nil, errors.New("state: decrypt: no identities")
	}
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(armored)), identities...)
	if err != nil {
		return nil, fmt.Errorf("state: decrypt: %w", err)
	}
	doc, err := io.ReadAll(io.LimitReader(r, maxPlaintext+1))
	if err != nil {
		return nil, fmt.Errorf("state: decrypt: %w", err)
	}
	if len(doc) > maxPlaintext {
		return nil, errors.New("state: decrypt: record exceeds size limit")
	}
	return Unmarshal(doc)
}

// EncodeRecord wraps armored ciphertext as a single-line encrypted record.
func EncodeRecord(armored []byte) []byte {
	enc := base64.RawStdEncoding
	out := make([]byte, len(Marker)+enc.EncodedLen(len(armored)))
	copy(out, Marker)
	enc.Encode(out[len(Marker):], armored)
	return out
}

// DecodeRecord unwraps a single-line encrypted record into armored
// ciphertext. Surrounding whitespace and base64 padding are tolerated.
func DecodeRecord(record []byte) ([]byte, error) {
	record = bytes.TrimSpace(record)
	if bytes.HasPrefix(record, []byte(PlainMarker)) {
		return nil, errors.New("state: record is unencrypted, not an encrypted record")
	}
	if !bytes.HasPrefix(record, []byte(Marker)) {
		return nil, errors.New("state: not a klotho record")
	}
	body := bytes.TrimRight(record[len(Marker):], "=")
	out := make([]byte, base64.RawStdEncoding.DecodedLen(len(body)))
	n, err := base64.RawStdEncoding.Decode(out, body)
	if err != nil {
		return nil, fmt.Errorf("state: decode record: %w", err)
	}
	return out[:n], nil
}

// IsRecord reports whether b carries a klotho record marker, encrypted or
// plain. It checks only the marker and does not validate the contents.
func IsRecord(b []byte) bool {
	b = bytes.TrimLeft(b, " \t\r\n")
	return bytes.HasPrefix(b, []byte(Marker)) || bytes.HasPrefix(b, []byte(PlainMarker))
}

// IsPlainRecord reports whether b carries the unencrypted record marker.
func IsPlainRecord(b []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(b, " \t\r\n"), []byte(PlainMarker))
}

// NewReservation returns a state that only claims a name.
func NewReservation() *cluster.State {
	return &cluster.State{Reserved: true, CreatedAt: time.Now().UTC()}
}

// ParseRecipients parses age public keys ("age1...").
func ParseRecipients(keys []string) ([]age.Recipient, error) {
	out := make([]age.Recipient, 0, len(keys))
	for i, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("state: recipient %d is empty", i)
		}
		rs, err := age.ParseRecipients(strings.NewReader(k))
		if err != nil {
			return nil, fmt.Errorf("state: recipient %d: %w", i, err)
		}
		out = append(out, rs...)
	}
	return out, nil
}

// ParseIdentities parses age secret keys ("AGE-SECRET-KEY-1..."). Errors
// never include key material.
func ParseIdentities(keys []string) ([]age.Identity, error) {
	out := make([]age.Identity, 0, len(keys))
	for i, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("state: identity %d is empty", i)
		}
		ids, err := age.ParseIdentities(strings.NewReader(k))
		if err != nil {
			return nil, fmt.Errorf("state: identity %d: %w", i, err)
		}
		out = append(out, ids...)
	}
	return out, nil
}

// LoadIdentitiesFromFile reads an age identity file: one secret key per
// line, with blank lines and lines starting with "#" ignored. CRLF line
// endings are accepted.
func LoadIdentitiesFromFile(path string) ([]age.Identity, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("state: read identity file: %w", err)
	}
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	ids, err := age.ParseIdentities(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("state: identity file %s: %w", path, err)
	}
	return ids, nil
}

// GenerateIdentity creates a new X25519 age identity.
func GenerateIdentity() (*age.X25519Identity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("state: generate identity: %w", err)
	}
	return id, nil
}
