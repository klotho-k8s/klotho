package gcp

import (
	"context"
	"fmt"

	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

// ComputeAPI is the subset of the GCP Compute Engine API klotho uses, reduced
// to plain request/response methods so it can be faked in tests (the generated
// google.golang.org/api client uses a call-builder pattern that does not fake
// cleanly). computeService, built by NewClient, wraps *compute.Service to
// satisfy it.
type ComputeAPI interface {
	// InsertInstance creates an instance in project/zone and returns the
	// zone operation that tracks it.
	InsertInstance(ctx context.Context, project, zone string, inst *compute.Instance) (*compute.Operation, error)
	// GetInstance returns one instance by name, or an error (a googleapi 404
	// when it does not exist).
	GetInstance(ctx context.Context, project, zone, name string) (*compute.Instance, error)
	// DeleteInstance deletes one instance by name and returns the zone
	// operation that tracks it.
	DeleteInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error)
	// ListInstancesByLabel returns every instance in the project (across all
	// zones) carrying label=value.
	ListInstancesByLabel(ctx context.Context, project, label, value string) ([]*compute.Instance, error)
	// WaitZoneOperation blocks until the given zone operation is DONE.
	WaitZoneOperation(ctx context.Context, project, zone, operation string) (*compute.Operation, error)

	// InsertGlobalAddress reserves a global (external) static IP and returns the
	// global operation that tracks it.
	InsertGlobalAddress(ctx context.Context, project string, addr *compute.Address) (*compute.Operation, error)
	// GetGlobalAddress returns one global address by name (a googleapi 404 when
	// it does not exist).
	GetGlobalAddress(ctx context.Context, project, name string) (*compute.Address, error)
	// DeleteGlobalAddress releases a global address by name and returns the
	// global operation that tracks it.
	DeleteGlobalAddress(ctx context.Context, project, name string) (*compute.Operation, error)
	// WaitGlobalOperation blocks until the given global operation is DONE.
	WaitGlobalOperation(ctx context.Context, project, operation string) (*compute.Operation, error)

	// GetImageFromFamily resolves the newest non-deprecated image in an image
	// project's family (for example debian-cloud / ubuntu-os-cloud).
	GetImageFromFamily(ctx context.Context, imageProject, family string) (*compute.Image, error)
	// GetImage returns one image by name from an image project.
	GetImage(ctx context.Context, imageProject, image string) (*compute.Image, error)
	// GetNetwork returns one VPC network by name.
	GetNetwork(ctx context.Context, project, network string) (*compute.Network, error)
	// GetSubnetwork returns one subnetwork by name in a region.
	GetSubnetwork(ctx context.Context, project, region, subnet string) (*compute.Subnetwork, error)
}

// computeService wraps the generated Compute client to satisfy ComputeAPI.
type computeService struct {
	svc *compute.Service
}

// NewClient builds a Compute client. When credsJSON is non-empty it authenticates
// with that service-account JSON; otherwise it uses Application Default
// Credentials (ADC: GOOGLE_APPLICATION_CREDENTIALS, gcloud auth, or the metadata
// server). It makes no API calls.
func NewClient(ctx context.Context, credsJSON []byte) (ComputeAPI, error) {
	var opts []option.ClientOption
	if len(credsJSON) > 0 {
		opts = append(opts, option.WithCredentialsJSON(credsJSON))
	}
	svc, err := compute.NewService(ctx, opts...)
	if err != nil {
		return nil, errorf("building the Compute client: %w", err)
	}
	return &computeService{svc: svc}, nil
}

func (c *computeService) InsertInstance(ctx context.Context, project, zone string, inst *compute.Instance) (*compute.Operation, error) {
	return c.svc.Instances.Insert(project, zone, inst).Context(ctx).Do()
}

func (c *computeService) GetInstance(ctx context.Context, project, zone, name string) (*compute.Instance, error) {
	return c.svc.Instances.Get(project, zone, name).Context(ctx).Do()
}

func (c *computeService) DeleteInstance(ctx context.Context, project, zone, name string) (*compute.Operation, error) {
	return c.svc.Instances.Delete(project, zone, name).Context(ctx).Do()
}

func (c *computeService) ListInstancesByLabel(ctx context.Context, project, label, value string) ([]*compute.Instance, error) {
	filter := fmt.Sprintf("labels.%s=%s", label, value)
	var out []*compute.Instance
	err := c.svc.Instances.AggregatedList(project).Filter(filter).Pages(ctx, func(page *compute.InstanceAggregatedList) error {
		for _, scoped := range page.Items {
			out = append(out, scoped.Instances...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *computeService) WaitZoneOperation(ctx context.Context, project, zone, operation string) (*compute.Operation, error) {
	return c.svc.ZoneOperations.Wait(project, zone, operation).Context(ctx).Do()
}

func (c *computeService) InsertGlobalAddress(ctx context.Context, project string, addr *compute.Address) (*compute.Operation, error) {
	return c.svc.GlobalAddresses.Insert(project, addr).Context(ctx).Do()
}

func (c *computeService) GetGlobalAddress(ctx context.Context, project, name string) (*compute.Address, error) {
	return c.svc.GlobalAddresses.Get(project, name).Context(ctx).Do()
}

func (c *computeService) DeleteGlobalAddress(ctx context.Context, project, name string) (*compute.Operation, error) {
	return c.svc.GlobalAddresses.Delete(project, name).Context(ctx).Do()
}

func (c *computeService) WaitGlobalOperation(ctx context.Context, project, operation string) (*compute.Operation, error) {
	return c.svc.GlobalOperations.Wait(project, operation).Context(ctx).Do()
}

func (c *computeService) GetImageFromFamily(ctx context.Context, imageProject, family string) (*compute.Image, error) {
	return c.svc.Images.GetFromFamily(imageProject, family).Context(ctx).Do()
}

func (c *computeService) GetImage(ctx context.Context, imageProject, image string) (*compute.Image, error) {
	return c.svc.Images.Get(imageProject, image).Context(ctx).Do()
}

func (c *computeService) GetNetwork(ctx context.Context, project, network string) (*compute.Network, error) {
	return c.svc.Networks.Get(project, network).Context(ctx).Do()
}

func (c *computeService) GetSubnetwork(ctx context.Context, project, region, subnet string) (*compute.Subnetwork, error) {
	return c.svc.Subnetworks.Get(project, region, subnet).Context(ctx).Do()
}
