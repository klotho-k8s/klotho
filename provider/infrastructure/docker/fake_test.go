package docker

import (
	"context"
	"fmt"
	"sync"
)

// fakeRuntime is an in-memory Runtime for tests: no daemon required. It
// records containers, networks and the commands exec'd into each container.
type fakeRuntime struct {
	mu              sync.Mutex
	networks        map[string]bool
	containers      map[string]*Container
	nextIP          int
	nextPort        int
	execs           map[string][][]string // container name -> list of argv
	ports           map[string]int        // container name -> published host port
	removed         []string
	removedNetworks []string
	cleanup         *CleanupOpts
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		networks:   map[string]bool{},
		containers: map[string]*Container{},
		execs:      map[string][][]string{},
		ports:      map[string]int{},
		nextIP:     2,
		nextPort:   35000,
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
	if opts.PublishAPIPort {
		f.ports[opts.Name] = f.nextPort
		f.nextPort++
	}
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

func (f *fakeRuntime) PublishedPort(_ context.Context, name string, _ int) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.ports[name]
	if !ok {
		return "", 0, fmt.Errorf("no published port for %s", name)
	}
	return "127.0.0.1", p, nil
}

func (f *fakeRuntime) RemoveNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case "", "podman", "bridge", "host", "none":
		return nil
	}
	delete(f.networks, name)
	f.removedNetworks = append(f.removedNetworks, name)
	return nil
}

func (f *fakeRuntime) RunCleanup(_ context.Context, opts CleanupOpts) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := opts
	f.cleanup = &o
	name := opts.Name
	f.containers[name] = &Container{
		ID: "id-" + name, Name: name, Running: true,
		Labels: map[string]string{CleanupLabelKey: opts.Cluster},
	}
	return "id-" + name, nil
}

func (f *fakeRuntime) execCount(container string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.execs[container])
}
