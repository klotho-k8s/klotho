package digitalocean

import (
	"context"
	"errors"
	"net/http"

	"github.com/digitalocean/godo"
)

// Client wraps the godo client to satisfy API, paginating the list calls and
// reducing godo's call surface to the plain methods klotho uses.
type Client struct {
	godo *godo.Client
}

// NewClient builds a DigitalOcean client authenticated with the configured
// token. It makes no API calls.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Token == "" {
		return nil, errorf("a DigitalOcean API token is required (set DIGITALOCEAN_TOKEN)")
	}
	return &Client{godo: godo.NewFromToken(cfg.Token)}, nil
}

func (c *Client) CreateDroplet(ctx context.Context, req *godo.DropletCreateRequest) (*godo.Droplet, error) {
	d, _, err := c.godo.Droplets.Create(ctx, req)
	return d, err
}

func (c *Client) GetDroplet(ctx context.Context, id int) (*godo.Droplet, error) {
	d, _, err := c.godo.Droplets.Get(ctx, id)
	return d, err
}

func (c *Client) DeleteDroplet(ctx context.Context, id int) error {
	_, err := c.godo.Droplets.Delete(ctx, id)
	return err
}

func (c *Client) ListDropletsByTag(ctx context.Context, tag string) ([]godo.Droplet, error) {
	var out []godo.Droplet
	opt := &godo.ListOptions{Page: 1, PerPage: 200}
	for {
		page, resp, err := c.godo.Droplets.ListByTag(ctx, tag, opt)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			return out, nil
		}
		pg, err := resp.Links.CurrentPage()
		if err != nil {
			return out, nil
		}
		opt.Page = pg + 1
	}
}

func (c *Client) CreateLoadBalancer(ctx context.Context, req *godo.LoadBalancerRequest) (*godo.LoadBalancer, error) {
	lb, _, err := c.godo.LoadBalancers.Create(ctx, req)
	return lb, err
}

func (c *Client) GetLoadBalancer(ctx context.Context, id string) (*godo.LoadBalancer, error) {
	lb, _, err := c.godo.LoadBalancers.Get(ctx, id)
	return lb, err
}

func (c *Client) DeleteLoadBalancer(ctx context.Context, id string) error {
	_, err := c.godo.LoadBalancers.Delete(ctx, id)
	return err
}

func (c *Client) ListRegions(ctx context.Context) ([]godo.Region, error) {
	var out []godo.Region
	opt := &godo.ListOptions{Page: 1, PerPage: 200}
	for {
		page, resp, err := c.godo.Regions.List(ctx, opt)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			return out, nil
		}
		pg, err := resp.Links.CurrentPage()
		if err != nil {
			return out, nil
		}
		opt.Page = pg + 1
	}
}

func (c *Client) ListSizes(ctx context.Context) ([]godo.Size, error) {
	var out []godo.Size
	opt := &godo.ListOptions{Page: 1, PerPage: 200}
	for {
		page, resp, err := c.godo.Sizes.List(ctx, opt)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			return out, nil
		}
		pg, err := resp.Links.CurrentPage()
		if err != nil {
			return out, nil
		}
		opt.Page = pg + 1
	}
}

func (c *Client) ListImages(ctx context.Context) ([]godo.Image, error) {
	var out []godo.Image
	opt := &godo.ListOptions{Page: 1, PerPage: 200}
	for {
		page, resp, err := c.godo.Images.ListDistribution(ctx, opt)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			return out, nil
		}
		pg, err := resp.Links.CurrentPage()
		if err != nil {
			return out, nil
		}
		opt.Page = pg + 1
	}
}

func (c *Client) GetImageBySlug(ctx context.Context, slug string) (*godo.Image, error) {
	img, _, err := c.godo.Images.GetBySlug(ctx, slug)
	return img, err
}

// isNotFound reports whether err is a DigitalOcean 404.
func isNotFound(err error) bool {
	var er *godo.ErrorResponse
	if errors.As(err, &er) && er.Response != nil {
		return er.Response.StatusCode == http.StatusNotFound
	}
	return false
}
