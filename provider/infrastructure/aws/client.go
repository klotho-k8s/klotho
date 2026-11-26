package aws

import (
	"context"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// NewClient builds an EC2 client from the default credential chain (env,
// shared profile, instance role), honoring Config.Region and Config.Profile.
// It makes no API calls. *ec2.Client satisfies EC2API.
func NewClient(ctx context.Context, cfg Config) (*ec2.Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}
	awscfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, errorf("loading AWS config: %w", err)
	}
	return ec2.NewFromConfig(awscfg), nil
}
