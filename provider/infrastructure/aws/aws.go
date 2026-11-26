// Package aws is the infrastructure provider for clusters managed by the
// Cluster API AWS provider (CAPA). It boots the bootstrap node as a single EC2
// instance with klotho's cloud-init as user-data, resolves the control plane
// endpoint, renders the AWSMachine that adopts the running instance, and tears
// everything down by tag.
//
// # Name and tag conventions
//
// Nothing this provider creates is recorded by ID alone; the following
// conventions are the contract Plan, Adopt and Teardown rely on:
//
//   - every resource it creates carries the tag ClusterTagKey / <cluster>
//     (klotho's own ownership tag, "klotho.io/cluster");
//   - the bootstrap instance additionally carries the tags CAPA matches on
//     when it adopts an existing instance instead of creating one: the
//     cluster-owned tag CAPAClusterTagKey(cluster) = "owned", the role tag
//     CAPARoleTagKey = "control-plane", and the Name tag = MachineName(cluster)
//     ("<cluster>-control-plane-0"), which is also the adopted AWSMachine's
//     name;
//   - the EC2 instance id is recorded in state as the bootstrap node's ID
//     (providerID "aws:///<az>/<id>"), and Plan/Adopt also re-find instances by
//     tag so a lost status update does not strand them.
//
// CAPA adoption (why the tags above): CAPA's AWSMachine controller resolves an
// instance before creating one. If the AWSMachine already has a providerID it
// calls InstanceIfExists; otherwise GetRunningInstanceByTags filters on the
// cluster-owned tag and the Name tag and adopts a running match. So a
// pre-created, running, correctly tagged instance is adopted rather than
// recreated — the same pattern CAPMOX and CAPD use. This is implemented from
// knowledge of CAPA's pkg/cloud/services/ec2/instances.go and MUST be confirmed
// against the pinned CAPA version and a live run (see the package tests and the
// PR notes).
package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/klotho-k8s/klotho/provider"
	"github.com/klotho-k8s/klotho/provider/infrastructure"
)

// ProviderName is the provider's identifier, as clusterctl knows CAPA.
const ProviderName = "aws"

// ClusterTagKey is klotho's own ownership tag key; its value is the cluster
// name. Every resource klotho creates for a cluster carries it.
const ClusterTagKey = "klotho.io/cluster"

// CAPA's well-known tag keys, matched when CAPA adopts an existing instance.
const (
	capaClusterTagPrefix = "sigs.k8s.io/cluster-api-provider-aws/cluster/"
	capaRoleTagKey       = "sigs.k8s.io/cluster-api-provider-aws/role"
	nameTagKey           = "Name"
	capaOwnedValue       = "owned"
	controlPlaneRole     = "control-plane"
)

// Defaults applied by the provider.
const (
	DefaultPort         = 6443
	DefaultInstanceType = "t3.large"
	DefaultLaunchPoll   = 5 * time.Second
	DefaultLaunchWait   = 5 * time.Minute
)

// Template variables read into the generated bundle. They are the names CAPA's
// cluster templates use; Prepare fills them from Config.
const (
	VarRegion            = "AWS_REGION"
	VarSSHKeyName        = "AWS_SSH_KEY_NAME"
	VarControlPlaneType  = "AWS_CONTROL_PLANE_MACHINE_TYPE"
	VarNodeType          = "AWS_NODE_MACHINE_TYPE"
	VarAMIID             = "AWS_AMI_ID"
	VarVPCID             = "AWS_VPC_ID"
	VarSubnetID          = "AWS_SUBNET_ID"
	VarControlPlaneCount = "CONTROL_PLANE_MACHINE_COUNT"
)

var _ infrastructure.Provider = (*Provider)(nil)
var _ provider.Checker = (*Provider)(nil)

// EC2API is the subset of the EC2 client klotho uses, so it can be faked in
// tests. *ec2.Client satisfies it.
type EC2API interface {
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, opts ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, opts ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	DescribeSecurityGroups(ctx context.Context, in *ec2.DescribeSecurityGroupsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DescribeKeyPairs(ctx context.Context, in *ec2.DescribeKeyPairsInput, opts ...func(*ec2.Options)) (*ec2.DescribeKeyPairsOutput, error)
	DescribeVpcs(ctx context.Context, in *ec2.DescribeVpcsInput, opts ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error)
	AllocateAddress(ctx context.Context, in *ec2.AllocateAddressInput, opts ...func(*ec2.Options)) (*ec2.AllocateAddressOutput, error)
	ReleaseAddress(ctx context.Context, in *ec2.ReleaseAddressInput, opts ...func(*ec2.Options)) (*ec2.ReleaseAddressOutput, error)
	AssociateAddress(ctx context.Context, in *ec2.AssociateAddressInput, opts ...func(*ec2.Options)) (*ec2.AssociateAddressOutput, error)
	DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, opts ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
}

// Config configures the provider. Zero fields fall back to defaults; see
// Resolve.
type Config struct {
	// Region is the AWS region, e.g. "us-east-1". Required.
	Region string
	// Profile selects a shared-config profile for the credential chain. Empty
	// uses the default chain (env, profile, instance role).
	Profile string

	// AMIID is the bootstrap image. Empty means AMIFilter resolves the latest
	// matching image.
	AMIID string
	// AMIOwner and AMINamePattern filter for an AMI when AMIID is empty.
	// Defaults resolve a recent Ubuntu 22.04 cloud image.
	AMIOwner       string
	AMINamePattern string

	// InstanceType sizes the bootstrap instance. Empty means
	// DefaultInstanceType.
	InstanceType string
	// SSHKeyName is an existing EC2 key pair for the instance and the template.
	SSHKeyName string
	// IAMInstanceProfile is attached to the bootstrap instance (CAPA's control
	// plane nodes normally use "control-plane.cluster-api-provider-aws.sigs.k8s.io").
	IAMInstanceProfile string

	// VPCID and SubnetID place the bootstrap instance. Both required for now:
	// creating a VPC/subnet/IGW/SG is not yet implemented (see Prepare).
	VPCID    string
	SubnetID string
	// SecurityGroupIDs are attached to the instance. Empty means the default
	// security group of the VPC is used (Resolve notes this).
	SecurityGroupIDs []string

	// ControlPlaneEndpoint is the API server host (an EIP, DNS name or
	// existing load balancer address). Empty with AllocateElasticIP allocates
	// an Elastic IP; empty without it is an error (the endpoint must be known
	// before the certificates are minted).
	ControlPlaneEndpoint string
	// AllocateElasticIP allocates an Elastic IP in Prepare, associates it with
	// the bootstrap instance in Launch, and uses it as the endpoint.
	AllocateElasticIP bool
	// Port is the API server port. Zero means DefaultPort.
	Port int
	// Spot requests the bootstrap instance (and, when the bundle supports it,
	// the control plane replicas) as one-time Spot instances. Development and
	// test only: a reclaimed Spot node is terminated, not restarted.
	Spot bool
	// PublicIP forces a public IP on the bootstrap instance, so a public
	// subnet needs no NAT gateway for the node to reach the internet.
	PublicIP bool
}

// Provider implements infrastructure.Provider for AWS via CAPA.
type Provider struct {
	ec2 EC2API
	cfg Config

	launchPoll time.Duration
	launchWait time.Duration
}

// New returns a provider talking to EC2 through client, normally built by
// NewClient.
func New(client EC2API, cfg Config) *Provider {
	return &Provider{ec2: client, cfg: cfg, launchPoll: DefaultLaunchPoll, launchWait: DefaultLaunchWait}
}

// Name implements infrastructure.Provider.
func (p *Provider) Name() string { return ProviderName }

// Tag returns klotho's ownership tag value for cluster (the cluster name); the
// key is ClusterTagKey.
func Tag(cluster string) string { return strings.ToLower(cluster) }

// MachineName is the bootstrap machine's name, shared by the adopted
// AWSMachine and the instance's Name tag. It matches the kubeadm control
// plane provider's BootstrapMachineName (<cluster>-bootstrap) so the Machine
// the control plane adopts resolves its infrastructureRef to this AWSMachine,
// and CAPA's GetRunningInstanceByTags (which filters on the Name tag equal to
// the AWSMachine name plus the cluster-owned tag) adopts the running instance.
func MachineName(cluster string) string { return cluster + "-bootstrap" }

// capaClusterTagKey is CAPA's cluster-owned tag key for a cluster.
func capaClusterTagKey(cluster string) string { return capaClusterTagPrefix + cluster }

// bootstrapTags returns the tags the bootstrap instance must carry: klotho's
// ownership tag plus the three CAPA matches on to adopt it.
func bootstrapTags(cluster string) map[string]string {
	return map[string]string{
		ClusterTagKey:              Tag(cluster),
		capaClusterTagKey(cluster): capaOwnedValue,
		capaRoleTagKey:             controlPlaneRole,
		nameTagKey:                 MachineName(cluster),
	}
}

func (p *Provider) port() int {
	if p.cfg.Port > 0 {
		return p.cfg.Port
	}
	return DefaultPort
}

func (p *Provider) instanceType() string {
	if p.cfg.InstanceType != "" {
		return p.cfg.InstanceType
	}
	return DefaultInstanceType
}

// Dependencies implements provider.Dependent: CAPA has no in-cluster
// dependency like the Proxmox provider's IPAM.
func (p *Provider) Dependencies() []provider.Dependency { return nil }

func errorf(format string, args ...any) error {
	return fmt.Errorf("aws provider: "+format, args...)
}

// ptr returns a pointer to v, for the SDK's pointer-heavy inputs.
func ptr[T any](v T) *T { return &v }

// providerID is the Kubernetes providerID CAPA sets on an adopted instance.
func providerID(az, instanceID string) string {
	return fmt.Sprintf("aws:///%s/%s", az, instanceID)
}

// awsString unwraps an SDK *string.
func awsString(s *string) string { return awssdk.ToString(s) }
