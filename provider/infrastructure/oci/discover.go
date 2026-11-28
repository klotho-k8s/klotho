package oci

import (
	"context"
	"errors"
	"strconv"

	"github.com/oracle/oci-go-sdk/v65/common"

	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// Discover reports the availability domains, compute shapes and images a cluster
// template can be prefilled with, within the configured compartment. It is
// read-only.
func (p *Provider) Discover(ctx context.Context) (infrastructure.Inventory, error) {
	if p.cfg.CompartmentID == "" {
		return nil, errorf("compartmentId is required for discovery (set oci.compartmentId)")
	}
	inv := infrastructure.Inventory{}

	ads, err := p.client.ListAvailabilityDomains(ctx, p.cfg.CompartmentID)
	if err != nil {
		return nil, errorf("listing availability domains: %w", err)
	}
	for _, ad := range ads {
		inv["availabilityDomain"] = append(inv["availabilityDomain"], infrastructure.Item{ID: ad.ID, Name: ad.Name})
	}

	shapes, err := p.client.ListShapes(ctx, p.cfg.CompartmentID)
	if err != nil {
		return nil, errorf("listing shapes: %w", err)
	}
	seenShape := map[string]bool{}
	for _, s := range shapes {
		name := strVal(s.Shape)
		if name == "" || seenShape[name] {
			continue
		}
		seenShape[name] = true
		attrs := map[string]string{}
		if s.Ocpus != nil {
			attrs["ocpus"] = strconv.FormatFloat(float64(*s.Ocpus), 'f', -1, 32)
		}
		if s.MemoryInGBs != nil {
			attrs["memoryGBs"] = strconv.FormatFloat(float64(*s.MemoryInGBs), 'f', -1, 32)
		}
		inv["shape"] = append(inv["shape"], infrastructure.Item{ID: name, Name: name, Attrs: attrs})
	}

	images, err := p.client.ListImages(ctx, p.cfg.CompartmentID)
	if err != nil {
		return nil, errorf("listing images: %w", err)
	}
	for _, img := range images {
		attrs := map[string]string{
			"os":        strVal(img.OperatingSystem),
			"osVersion": strVal(img.OperatingSystemVersion),
		}
		inv["image"] = append(inv["image"], infrastructure.Item{ID: strVal(img.Id), Name: strVal(img.DisplayName), Attrs: attrs})
	}

	return inv, nil
}

// isNotFound reports whether err is an OCI 404 service error.
func isNotFound(err error) bool {
	var svc common.ServiceError
	return errors.As(err, &svc) && svc.GetHTTPStatusCode() == 404
}
