package proxmox

import (
	"reflect"
	"testing"
)

func TestParseTags(t *testing.T) {
	cases := map[string][]string{
		"":                         nil,
		"klotho":                   {"klotho"},
		"klotho;dev;cp":            {"klotho", "dev", "cp"},
		" klotho ; dev ;; ":        {"klotho", "dev"},
		"a,b c;d":                  {"a", "b", "c", "d"},
		"Klotho;klotho;dev":        {"Klotho", "dev"},
		"cluster-a;k8s.io_role+cp": {"cluster-a", "k8s.io_role+cp"},
	}
	for in, want := range cases {
		if got := parseTags(in); !reflect.DeepEqual(got, want) {
			t.Errorf("parseTags(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func TestJoinTags(t *testing.T) {
	if got := joinTags([]string{"klotho", " dev ", "", "cp"}); got != "klotho;dev;cp" {
		t.Errorf("joinTags = %q", got)
	}
	if got := joinTags(nil); got != "" {
		t.Errorf("joinTags(nil) = %q", got)
	}
	tags := []string{"klotho", "dev", "cp"}
	if got := parseTags(joinTags(tags)); !reflect.DeepEqual(got, tags) {
		t.Errorf("round trip = %v", got)
	}
	if !hasTag(tags, "DEV") || hasTag(tags, "ops") {
		t.Error("hasTag")
	}
}
