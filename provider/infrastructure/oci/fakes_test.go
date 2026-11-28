package oci

import (
	"context"
	"fmt"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
)

// fakeOCI is an in-memory API for tests. It records the calls that matter and
// serves instances so Launch/Plan/Teardown/Adopt can be exercised without OCI.
type fakeOCI struct {
	instances map[string]*core.Instance
	nextID    int

	launchDetails *core.LaunchInstanceDetails
	terminated    []string

	images []core.Image
	shapes []core.Shape
	ads    []AvailabilityDomain

	addresses []string
}

func newFakeOCI() *fakeOCI {
	return &fakeOCI{
		instances: map[string]*core.Instance{},
		nextID:    1,
		images: []core.Image{{
			Id:                     common.String("ocid1.image.oc1..ubuntu"),
			DisplayName:            common.String("Canonical-Ubuntu-22.04"),
			OperatingSystem:        common.String("Canonical Ubuntu"),
			OperatingSystemVersion: common.String("22.04"),
		}},
		shapes: []core.Shape{{
			Shape:       common.String("VM.Standard.E4.Flex"),
			Ocpus:       common.Float32(2),
			MemoryInGBs: common.Float32(16),
		}},
		ads:       []AvailabilityDomain{{Name: "Uocm:US-ASHBURN-AD-1", ID: "ocid1.ad..1"}},
		addresses: []string{"203.0.113.10", "10.0.0.10"},
	}
}

func (f *fakeOCI) LaunchInstance(_ context.Context, details core.LaunchInstanceDetails) (core.Instance, error) {
	f.launchDetails = &details
	id := fmt.Sprintf("ocid1.instance.oc1..%04d", f.nextID)
	f.nextID++
	inst := core.Instance{
		Id:                 common.String(id),
		DisplayName:        details.DisplayName,
		CompartmentId:      details.CompartmentId,
		AvailabilityDomain: details.AvailabilityDomain,
		Shape:              details.Shape,
		FreeformTags:       details.FreeformTags,
		LifecycleState:     core.InstanceLifecycleStateRunning,
	}
	f.instances[id] = &inst
	return inst, nil
}

func (f *fakeOCI) GetInstance(_ context.Context, instanceID string) (core.Instance, error) {
	if inst, ok := f.instances[instanceID]; ok {
		return *inst, nil
	}
	return core.Instance{}, notFoundErr()
}

func (f *fakeOCI) ListInstances(_ context.Context, _ string) ([]core.Instance, error) {
	out := make([]core.Instance, 0, len(f.instances))
	for _, inst := range f.instances {
		out = append(out, *inst)
	}
	return out, nil
}

func (f *fakeOCI) TerminateInstance(_ context.Context, instanceID string) error {
	if _, ok := f.instances[instanceID]; !ok {
		return notFoundErr()
	}
	f.terminated = append(f.terminated, instanceID)
	delete(f.instances, instanceID)
	return nil
}

func (f *fakeOCI) PrimaryVnicAddresses(_ context.Context, _, _ string) ([]string, error) {
	return f.addresses, nil
}

func (f *fakeOCI) ListImages(_ context.Context, _ string) ([]core.Image, error) {
	return f.images, nil
}

func (f *fakeOCI) ListShapes(_ context.Context, _ string) ([]core.Shape, error) {
	return f.shapes, nil
}

func (f *fakeOCI) ListAvailabilityDomains(_ context.Context, _ string) ([]AvailabilityDomain, error) {
	return f.ads, nil
}

// seedInstance adds a running instance with the given id, display name and
// freeform tags.
func (f *fakeOCI) seedInstance(id, displayName string, tags map[string]string) {
	f.instances[id] = &core.Instance{
		Id:             common.String(id),
		DisplayName:    common.String(displayName),
		FreeformTags:   tags,
		LifecycleState: core.InstanceLifecycleStateRunning,
	}
}

// svcErr is a minimal common.ServiceError for tests.
type svcErr struct{ status int }

func (e svcErr) Error() string           { return fmt.Sprintf("service error %d", e.status) }
func (e svcErr) GetHTTPStatusCode() int  { return e.status }
func (e svcErr) GetMessage() string      { return "not found" }
func (e svcErr) GetCode() string         { return "NotFound" }
func (e svcErr) GetOpcRequestID() string { return "" }

var _ common.ServiceError = svcErr{}

// notFoundErr returns an OCI 404 service error, as isNotFound detects.
func notFoundErr() error { return svcErr{status: 404} }
