package digitalocean

import (
	"context"
	"net/http"

	"github.com/digitalocean/godo"
)

// fakeDO is an in-memory API for tests. It records the calls that matter and
// serves droplets and load balancers so Prepare/Launch/Plan/Teardown/Adopt can
// be exercised without DigitalOcean.
type fakeDO struct {
	regions []godo.Region
	sizes   []godo.Size
	images  []godo.Image

	droplets map[int]*godo.Droplet
	nextID   int

	lbs    map[string]*godo.LoadBalancer
	nextLB int

	createReq *godo.DropletCreateRequest
	createdLB *godo.LoadBalancerRequest
	deleted   []int
	deletedLB []string
}

func newFakeDO() *fakeDO {
	return &fakeDO{
		droplets: map[int]*godo.Droplet{},
		nextID:   1001,
		lbs:      map[string]*godo.LoadBalancer{},
		nextLB:   1,
		regions:  []godo.Region{{Slug: "nyc3", Name: "New York 3", Available: true}},
		sizes:    []godo.Size{{Slug: "s-2vcpu-4gb", Vcpus: 2, Memory: 4096, PriceMonthly: 24, Available: true}},
		images:   []godo.Image{{ID: 1, Name: "Ubuntu 22.04 x64", Slug: "ubuntu-22-04-x64", Distribution: "Ubuntu"}},
	}
}

func notFoundErr() error {
	return &godo.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}, Message: "not found"}
}

func (f *fakeDO) CreateDroplet(_ context.Context, req *godo.DropletCreateRequest) (*godo.Droplet, error) {
	f.createReq = req
	id := f.nextID
	f.nextID++
	d := &godo.Droplet{
		ID:     id,
		Name:   req.Name,
		Status: "active",
		Tags:   req.Tags,
		Networks: &godo.Networks{V4: []godo.NetworkV4{
			{IPAddress: "203.0.113.10", Type: "public"},
			{IPAddress: "10.0.0.10", Type: "private"},
		}},
	}
	f.droplets[id] = d
	return d, nil
}

func (f *fakeDO) GetDroplet(_ context.Context, id int) (*godo.Droplet, error) {
	if d, ok := f.droplets[id]; ok {
		return d, nil
	}
	return nil, notFoundErr()
}

func (f *fakeDO) DeleteDroplet(_ context.Context, id int) error {
	if _, ok := f.droplets[id]; !ok {
		return notFoundErr()
	}
	f.deleted = append(f.deleted, id)
	delete(f.droplets, id)
	return nil
}

func (f *fakeDO) ListDropletsByTag(_ context.Context, tag string) ([]godo.Droplet, error) {
	var out []godo.Droplet
	for _, d := range f.droplets {
		for _, t := range d.Tags {
			if t == tag {
				out = append(out, *d)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeDO) CreateLoadBalancer(_ context.Context, req *godo.LoadBalancerRequest) (*godo.LoadBalancer, error) {
	f.createdLB = req
	id := "lb-" + req.Name
	f.nextLB++
	lb := &godo.LoadBalancer{
		ID:     id,
		Name:   req.Name,
		IP:     "198.51.100.5",
		Status: "active",
		Tag:    req.Tag,
	}
	f.lbs[id] = lb
	return lb, nil
}

func (f *fakeDO) GetLoadBalancer(_ context.Context, id string) (*godo.LoadBalancer, error) {
	if lb, ok := f.lbs[id]; ok {
		return lb, nil
	}
	return nil, notFoundErr()
}

func (f *fakeDO) DeleteLoadBalancer(_ context.Context, id string) error {
	if _, ok := f.lbs[id]; !ok {
		return notFoundErr()
	}
	f.deletedLB = append(f.deletedLB, id)
	delete(f.lbs, id)
	return nil
}

func (f *fakeDO) ListRegions(_ context.Context) ([]godo.Region, error) { return f.regions, nil }
func (f *fakeDO) ListSizes(_ context.Context) ([]godo.Size, error)     { return f.sizes, nil }
func (f *fakeDO) ListImages(_ context.Context) ([]godo.Image, error)   { return f.images, nil }

func (f *fakeDO) GetImageBySlug(_ context.Context, slug string) (*godo.Image, error) {
	for i := range f.images {
		if f.images[i].Slug == slug {
			return &f.images[i], nil
		}
	}
	return nil, notFoundErr()
}

// seedDroplet adds an active droplet with the given id, name and tags.
func (f *fakeDO) seedDroplet(id int, name string, tags []string) {
	f.droplets[id] = &godo.Droplet{
		ID:     id,
		Name:   name,
		Status: "active",
		Tags:   tags,
		Networks: &godo.Networks{V4: []godo.NetworkV4{
			{IPAddress: "203.0.113.10", Type: "public"},
		}},
	}
}

// seedLB adds an active load balancer with the given id and IP.
func (f *fakeDO) seedLB(id, ip string) {
	f.lbs[id] = &godo.LoadBalancer{ID: id, Name: id, IP: ip, Status: "active"}
}
