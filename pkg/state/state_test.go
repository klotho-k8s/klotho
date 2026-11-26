package state

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/klotho-k8s/klotho/pkg/cluster"
)

func sampleState() *cluster.State {
	return &cluster.State{
		Spec: cluster.Spec{
			Name:      "dev",
			Provider:  "docker",
			Variables: map[string]string{"K": "v"},
		},
		Endpoint:  cluster.Endpoint{Host: "10.0.0.1", Port: 6443},
		Bootstrap: &cluster.Node{ID: "i-1", Name: "cp-0", Addresses: []string{"10.0.0.2"}},
		Resources: []cluster.Resource{{Kind: "vm", ID: "i-1", Name: "cp-0"}},
		CA:        cluster.KeyPair{Cert: []byte("-----BEGIN CERTIFICATE-----\nx\n"), Key: []byte("key\n")},
		CreatedAt: time.Date(2026, 11, 22, 10, 0, 0, 123, time.UTC),
	}
}

func newID(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func codecFor(t *testing.T, recipients []*age.X25519Identity, identities []*age.X25519Identity) *Codec {
	t.Helper()
	var rs []age.Recipient
	for _, id := range recipients {
		rs = append(rs, id.Recipient())
	}
	var ids []age.Identity
	for _, id := range identities {
		ids = append(ids, id)
	}
	c, err := NewCodec(rs, ids)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func assertSingleLine(t *testing.T, rec []byte) {
	t.Helper()
	if bytes.ContainsAny(rec, "\r\n") {
		t.Fatalf("record is not a single line: %q", rec)
	}
}

func TestRoundTripOneRecipient(t *testing.T) {
	id := newID(t)
	c := codecFor(t, []*age.X25519Identity{id}, []*age.X25519Identity{id})
	want := sampleState()
	rec, err := c.Seal(want)
	if err != nil {
		t.Fatal(err)
	}
	assertSingleLine(t, rec)
	if !strings.HasPrefix(string(rec), Marker) {
		t.Fatalf("record lacks marker: %.40q", rec)
	}
	got, err := c.Open(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestRoundTripTwoRecipientsSecondIdentity(t *testing.T) {
	a, b := newID(t), newID(t)
	sealer := codecFor(t, []*age.X25519Identity{a, b}, nil)
	rec, err := sealer.Seal(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	opener := codecFor(t, nil, []*age.X25519Identity{b})
	got, err := opener.Open(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, sampleState()) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestWrongIdentityFails(t *testing.T) {
	a, other := newID(t), newID(t)
	rec, err := codecFor(t, []*age.X25519Identity{a}, nil).Seal(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	_, err = codecFor(t, nil, []*age.X25519Identity{other}).Open(rec)
	var noMatch *age.NoIdentityMatchError
	if !errors.As(err, &noMatch) {
		t.Fatalf("want NoIdentityMatchError, got %v", err)
	}
}

func TestOpenWithoutIdentitiesFails(t *testing.T) {
	a := newID(t)
	c := codecFor(t, []*age.X25519Identity{a}, nil)
	rec, err := c.Seal(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(rec); err == nil {
		t.Fatal("open without identities succeeded")
	}
}

func TestTamperedRecordFails(t *testing.T) {
	id := newID(t)
	c := codecFor(t, []*age.X25519Identity{id}, []*age.X25519Identity{id})
	rec, err := c.Seal(sampleState())
	if err != nil {
		t.Fatal(err)
	}

	// Flip a bit in the binary ciphertext body and re-wrap it validly, so
	// only age's authentication can catch it.
	armored, err := DecodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	bin, err := io.ReadAll(armor.NewReader(bytes.NewReader(armored)))
	if err != nil {
		t.Fatal(err)
	}
	bin[len(bin)-5] ^= 0x01
	var buf bytes.Buffer
	w := armor.NewWriter(&buf)
	if _, err := w.Write(bin); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(EncodeRecord(buf.Bytes())); err == nil {
		t.Fatal("tampered ciphertext opened")
	}

	// Damage the record text itself.
	bad := append([]byte(nil), rec...)
	bad[len(bad)/2] = '!'
	if _, err := c.Open(bad); err == nil {
		t.Fatal("damaged record opened")
	}
	if _, err := c.Open(rec[:len(rec)-20]); err == nil {
		t.Fatal("truncated record opened")
	}
}

func TestVersionMismatchFails(t *testing.T) {
	_, err := Unmarshal([]byte(`{"version":2,"kind":"klotho.state","state":{}}`))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("want ErrUnsupportedVersion, got %v", err)
	}
	if _, err := Unmarshal([]byte(`{"version":1,"kind":"other","state":{}}`)); err == nil {
		t.Fatal("wrong kind accepted")
	}
	if _, err := Unmarshal([]byte(`{"version":1,"kind":"klotho.state"}`)); err == nil {
		t.Fatal("missing payload accepted")
	}

	// The same check applies through both codecs.
	plain := []byte(PlainMarker + `{"version":99,"kind":"klotho.state","state":{}}`)
	if _, err := (PlainCodec{}).Open(plain); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("plain: want ErrUnsupportedVersion, got %v", err)
	}
	id := newID(t)
	var buf bytes.Buffer
	aw := armor.NewWriter(&buf)
	w, err := age.Encrypt(aw, id.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, `{"version":99,"kind":"klotho.state","state":{}}`); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	c := codecFor(t, nil, []*age.X25519Identity{id})
	if _, err := c.Open(EncodeRecord(buf.Bytes())); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("encrypted: want ErrUnsupportedVersion, got %v", err)
	}
}

func TestIsRecord(t *testing.T) {
	id := newID(t)
	rec, err := codecFor(t, []*age.X25519Identity{id}, nil).Seal(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	plain, err := PlainCodec{}.Seal(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		in           []byte
		rec, isPlain bool
	}{
		"encrypted":       {rec, true, false},
		"plain":           {plain, true, true},
		"empty":           {nil, false, false},
		"text":            {[]byte("hello world"), false, false},
		"near miss":       {[]byte("klotho/v2:abc"), false, false},
		"no colon":        {[]byte("klotho/v1"), false, false},
		"leading newline": {append([]byte("\n"), rec...), true, false},
	} {
		if got := IsRecord(tc.in); got != tc.rec {
			t.Errorf("%s: IsRecord = %v, want %v", name, got, tc.rec)
		}
		if got := IsPlainRecord(tc.in); got != tc.isPlain {
			t.Errorf("%s: IsPlainRecord = %v, want %v", name, got, tc.isPlain)
		}
	}
}

func TestPlainCodecRoundTrip(t *testing.T) {
	want := sampleState()
	rec, err := PlainCodec{}.Seal(want)
	if err != nil {
		t.Fatal(err)
	}
	assertSingleLine(t, rec)
	if !strings.HasPrefix(string(rec), PlainMarker) {
		t.Fatalf("record lacks plain marker: %.40q", rec)
	}
	got, err := PlainCodec{}.Open(append(rec, '\n'))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestCodecsRefuseEachOther(t *testing.T) {
	id := newID(t)
	c := codecFor(t, []*age.X25519Identity{id}, []*age.X25519Identity{id})
	enc, err := c.Seal(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	plain, err := PlainCodec{}.Seal(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (PlainCodec{}).Open(enc); err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("plain codec opened encrypted record: %v", err)
	}
	if _, err := c.Open(plain); err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("encrypted codec opened plain record: %v", err)
	}
	if _, err := c.Open([]byte("not a record")); err == nil {
		t.Fatal("opened garbage")
	}
	if _, err := (PlainCodec{}).Open([]byte("not a record")); err == nil {
		t.Fatal("plain opened garbage")
	}
}

func TestReservationRoundTrip(t *testing.T) {
	r := NewReservation()
	if !r.Reserved || r.CreatedAt.IsZero() {
		t.Fatalf("bad reservation: %+v", r)
	}
	id := newID(t)
	for name, c := range map[string]RecordCodec{
		"encrypted": codecFor(t, []*age.X25519Identity{id}, []*age.X25519Identity{id}),
		"plain":     PlainCodec{},
	} {
		rec, err := c.Seal(r)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := c.Open(rec)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !got.Reserved || !got.CreatedAt.Equal(r.CreatedAt) || got.Bootstrap != nil {
			t.Fatalf("%s: reservation mismatch: %+v", name, got)
		}
	}
}

func TestSealNilAndNoRecipients(t *testing.T) {
	id := newID(t)
	if _, err := codecFor(t, []*age.X25519Identity{id}, nil).Seal(nil); err == nil {
		t.Fatal("sealed nil state")
	}
	if _, err := codecFor(t, nil, []*age.X25519Identity{id}).Seal(sampleState()); err == nil {
		t.Fatal("sealed with no recipients")
	}
	if _, err := NewCodec(nil, nil); err == nil {
		t.Fatal("empty codec accepted")
	}
}

func TestParseKeys(t *testing.T) {
	a, b := newID(t), newID(t)
	rs, err := ParseRecipients([]string{a.Recipient().String(), " " + b.Recipient().String() + "\n"})
	if err != nil || len(rs) != 2 {
		t.Fatalf("ParseRecipients: %v, %d", err, len(rs))
	}
	ids, err := ParseIdentities([]string{b.String()})
	if err != nil || len(ids) != 1 {
		t.Fatalf("ParseIdentities: %v, %d", err, len(ids))
	}
	armored, err := Encrypt(sampleState(), rs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(armored, ids); err != nil {
		t.Fatal(err)
	}

	if _, err := ParseRecipients([]string{"nope"}); err == nil {
		t.Fatal("bad recipient accepted")
	}
	if _, err := ParseRecipients([]string{""}); err == nil {
		t.Fatal("empty recipient accepted")
	}
	if _, err := ParseRecipients([]string{a.String()}); err == nil {
		t.Fatal("secret key accepted as recipient")
	}
	_, err = ParseIdentities([]string{"AGE-SECRET-KEY-1BOGUS"})
	if err == nil {
		t.Fatal("bad identity accepted")
	}
	if strings.Contains(err.Error(), "BOGUS") {
		t.Fatalf("error leaks key material: %v", err)
	}
	if _, err := ParseIdentities([]string{a.Recipient().String()}); err == nil {
		t.Fatal("public key accepted as identity")
	}
}

func TestLoadIdentitiesFromFile(t *testing.T) {
	a, b := newID(t), newID(t)
	content := "# created: today\r\n# public key: " + a.Recipient().String() + "\r\n" +
		a.String() + "\r\n\r\n" + b.String() + "\n"
	path := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ids, err := LoadIdentitiesFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("got %d identities, want 2", len(ids))
	}
	rec, err := codecFor(t, []*age.X25519Identity{b}, nil).Seal(sampleState())
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCodec(nil, ids)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(rec); err != nil {
		t.Fatal(err)
	}

	empty := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(empty, []byte("# nothing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentitiesFromFile(empty); err == nil {
		t.Fatal("file with no keys accepted")
	}
	if _, err := LoadIdentitiesFromFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestDecodeRecordAcceptsPadding(t *testing.T) {
	in := []byte("abcd")
	rec := EncodeRecord(in)
	got, err := DecodeRecord(append(append([]byte(nil), rec...), "==\n"...))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, in) {
		t.Fatalf("got %q", got)
	}
}
