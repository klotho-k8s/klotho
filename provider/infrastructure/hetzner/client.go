package hetzner

import (
	"context"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// API is the subset of the hcloud client klotho uses, so it can be faked in
// tests. hcloudAPI (built by NewClient) satisfies it.
type API interface {
	CreateServer(ctx context.Context, opts hcloud.ServerCreateOpts) (hcloud.ServerCreateResult, error)
	GetServer(ctx context.Context, id int64) (*hcloud.Server, error)
	ListServers(ctx context.Context, selector string) ([]*hcloud.Server, error)
	DeleteServer(ctx context.Context, id int64) error
	GetServerType(ctx context.Context, name string) (*hcloud.ServerType, error)
	GetImage(ctx context.Context, name string) (*hcloud.Image, error)
	GetLocation(ctx context.Context, name string) (*hcloud.Location, error)
	GetSSHKey(ctx context.Context, name string) (*hcloud.SSHKey, error)
	ListServerTypes(ctx context.Context) ([]*hcloud.ServerType, error)
	ListImages(ctx context.Context) ([]*hcloud.Image, error)
	ListLocations(ctx context.Context) ([]*hcloud.Location, error)
}

// hcloudAPI adapts *hcloud.Client to API, dropping the *hcloud.Response the
// sub-clients return.
type hcloudAPI struct{ c *hcloud.Client }

// NewClient builds an hcloud-backed API authenticated with the configured token.
// It makes no API calls.
func NewClient(cfg Config) (API, error) {
	if cfg.Token == "" {
		return nil, errorf("a Hetzner Cloud API token is required (set HCLOUD_TOKEN)")
	}
	return hcloudAPI{c: hcloud.NewClient(hcloud.WithToken(cfg.Token))}, nil
}

func (a hcloudAPI) CreateServer(ctx context.Context, opts hcloud.ServerCreateOpts) (hcloud.ServerCreateResult, error) {
	res, _, err := a.c.Server.Create(ctx, opts)
	return res, err
}

func (a hcloudAPI) GetServer(ctx context.Context, id int64) (*hcloud.Server, error) {
	s, _, err := a.c.Server.GetByID(ctx, id)
	return s, err
}

func (a hcloudAPI) ListServers(ctx context.Context, selector string) ([]*hcloud.Server, error) {
	return a.c.Server.AllWithOpts(ctx, hcloud.ServerListOpts{ListOpts: hcloud.ListOpts{LabelSelector: selector}})
}

func (a hcloudAPI) DeleteServer(ctx context.Context, id int64) error {
	_, _, err := a.c.Server.DeleteWithResult(ctx, &hcloud.Server{ID: id})
	return err
}

func (a hcloudAPI) GetServerType(ctx context.Context, name string) (*hcloud.ServerType, error) {
	t, _, err := a.c.ServerType.GetByName(ctx, name)
	return t, err
}

func (a hcloudAPI) GetImage(ctx context.Context, name string) (*hcloud.Image, error) {
	img, _, err := a.c.Image.GetByName(ctx, name)
	return img, err
}

func (a hcloudAPI) GetLocation(ctx context.Context, name string) (*hcloud.Location, error) {
	loc, _, err := a.c.Location.GetByName(ctx, name)
	return loc, err
}

func (a hcloudAPI) GetSSHKey(ctx context.Context, name string) (*hcloud.SSHKey, error) {
	k, _, err := a.c.SSHKey.GetByName(ctx, name)
	return k, err
}

func (a hcloudAPI) ListServerTypes(ctx context.Context) ([]*hcloud.ServerType, error) {
	return a.c.ServerType.All(ctx)
}

func (a hcloudAPI) ListImages(ctx context.Context) ([]*hcloud.Image, error) {
	return a.c.Image.All(ctx)
}

func (a hcloudAPI) ListLocations(ctx context.Context) ([]*hcloud.Location, error) {
	return a.c.Location.All(ctx)
}
