package docker

import (
	"context"
	"fmt"
	"sync"
)

// fakeRuntime is an in-memory Runtime for tests: no daemon required. It
// records containers, networks and the commands exec'd into each container.
type fakeRuntime struct {
	mu         sync.Mutex
	networks   map[string]bool
	containers map[string]*Container
	nextIP     int
	execs      map[string][][]string // container name -> list of argv
	removed    []string
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		networks:   map[string]bool{},
		containers: map[string]*Container{},
		execs:      map[string][][]string{},
		nextIP:     2,
	}
}

func (f *fakeRuntime) EnsureNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks[name] = true
	return nil
}

func (f *fakeRuntime) RunNode(_ context.Context, opts RunOpts) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[opts.Name]; ok {
		return "", fmt.Errorf("container %s already exists", opts.Name)
	}
	ip := fmt.Sprintf("172.18.0.%d", f.nextIP)
	f.nextIP++
	f.containers[opts.Name] = &Container{
		ID: "id-" + opts.Name, Name: opts.Name, Image: opts.Image,
		Running: true, Labels: opts.Labels, IP: ip,
	}
	return "id-" + opts.Name, nil
}

func (f *fakeRuntime) Exec(_ context.Context, name string, cmd []string, _ []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[name]; !ok {
		return "", fmt.Errorf("no such container %s", name)
	}
	f.execs[name] = append(f.execs[name], cmd)
	return "", nil
}

func (f *fakeRuntime) List(_ context.Context, selector map[string]string) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Container
	for _, c := range f.containers {
		match := true
		for k, v := range selector {
			if c.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (f *fakeRuntime) Inspect(_ context.Context, name string) (Container, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	if !ok {
		return Container{}, false, nil
	}
	return *c, true, nil
}

func (f *fakeRuntime) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeRuntime) execCount(container string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.execs[container])
}
