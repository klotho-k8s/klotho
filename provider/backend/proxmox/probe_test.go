package proxmox

import (
	"context"
	"errors"
	"testing"
)

func TestProbeCapsAt8KiB(t *testing.T) {
	f := newFake()
	f.maxComment = 8 << 10
	res, err := Probe(context.Background(), f.client(t), "klotho-probe")
	if err != nil {
		t.Fatal(err)
	}
	if res.MaxSize != 8192 {
		t.Errorf("MaxSize = %d, want 8192", res.MaxSize)
	}
	if !res.NewlinesSurvive {
		t.Error("NewlinesSurvive = false, want true for a faithful fake")
	}
	if len(res.Errors) != 4 {
		t.Errorf("Errors = %q, want one per size over the cap", res.Errors)
	}
	if f.has("klotho-probe") {
		t.Fatal("temporary pool not deleted")
	}
}

func TestProbeCommentLimit(t *testing.T) {
	f := newFake()
	f.maxComment = 8 << 10
	maxOK, err := ProbeCommentLimit(context.Background(), f.client(t), "klotho-probe", []int{100, 8192, 8193})
	if err != nil || maxOK != 8192 {
		t.Fatalf("ProbeCommentLimit = %d, %v", maxOK, err)
	}
	if f.has("klotho-probe") {
		t.Fatal("temporary pool not deleted")
	}
}

func TestProbeNewlinesStripped(t *testing.T) {
	f := newFake()
	f.stripNewlines = true
	res, err := Probe(context.Background(), f.client(t), "klotho-probe")
	if err != nil {
		t.Fatal(err)
	}
	if res.NewlinesSurvive || res.MaxSize != 128<<10 {
		t.Fatalf("result = %+v", res)
	}
	if f.has("klotho-probe") {
		t.Fatal("temporary pool not deleted")
	}
}

func TestProbeDeletesOnFailure(t *testing.T) {
	f := newFake()
	f.maxComment = 8 << 10
	f.failGet = true
	res, err := Probe(context.Background(), f.client(t), "klotho-probe")
	if err != nil {
		t.Fatal(err)
	}
	if res.MaxSize != 0 || res.NewlinesSurvive || len(res.Errors) != len(ProbeSizes)+1 {
		t.Fatalf("result = %+v", res)
	}
	if f.has("klotho-probe") {
		t.Fatal("temporary pool not deleted after failures")
	}
}

func TestProbeDeletesOnCancel(t *testing.T) {
	f := newFake()
	client := f.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ProbeCommentLimit(ctx, client, "klotho-probe", []int{1024})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if f.has("klotho-probe") {
		t.Fatal("temporary pool not deleted after cancellation")
	}
}

func TestProbeRefusesExistingPool(t *testing.T) {
	f := newFake()
	f.put(pool{ID: "klotho-probe", Comment: "someone's"})
	if _, err := Probe(context.Background(), f.client(t), "klotho-probe"); err == nil {
		t.Fatal("Probe used an existing pool")
	}
	if !f.has("klotho-probe") {
		t.Fatal("existing pool deleted")
	}
}

func TestProbeInvalidPoolID(t *testing.T) {
	f := newFake()
	if _, err := Probe(context.Background(), f.client(t), "bad/id"); err == nil {
		t.Fatal("invalid pool ID accepted")
	}
	if len(f.callLog()) != 0 {
		t.Fatal("invalid pool ID reached the server")
	}
}
