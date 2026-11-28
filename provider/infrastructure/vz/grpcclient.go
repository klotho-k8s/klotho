package vz

import (
	"context"
	"crypto/tls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	pb "github.com/klotho-k8s/klotho/provider/infrastructure/vz/vzagentv1"
)

// grpcClient is the production Client: a gRPC connection to the capvz-daemon over
// TLS with a bearer token, exactly the transport the Cluster API vz controller
// uses. It is pure Go (no cgo) and never links Virtualization.framework; the
// daemon on the Mac does the real work.
type grpcClient struct {
	conn *grpc.ClientConn
	rpc  pb.VZAgentClient
}

// NewClient dials the capvz-daemon described by cfg (Endpoint over TLS, trusting
// cfg.CA or the system roots, authenticating with cfg.Token) and returns a
// Client talking to it. grpc.NewClient is lazy: the TCP/TLS connection is not
// opened until the first RPC.
func NewClient(cfg Config) (Client, error) {
	if cfg.Endpoint == "" {
		return nil, errorf("no daemon endpoint: set vz.endpoint to the capvz-daemon's gRPC address (host:port)")
	}
	tlsOpt, err := clientTLS(cfg.CA, cfg.ServerName)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.Endpoint,
		tlsOpt,
		grpc.WithPerRPCCredentials(tokenCreds{token: cfg.Token}),
	)
	if err != nil {
		return nil, errorf("dialing capvz-daemon at %s: %w", cfg.Endpoint, err)
	}
	return &grpcClient{conn: conn, rpc: pb.NewVZAgentClient(conn)}, nil
}

// clientTLS returns a DialOption trusting the CA in caFile, or the host's system
// root CAs when caFile is empty. serverName, when set, overrides the TLS server
// name verified against the daemon's certificate.
func clientTLS(caFile, serverName string) (grpc.DialOption, error) {
	if caFile == "" {
		return grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: serverName,
		})), nil
	}
	creds, err := credentials.NewClientTLSFromFile(caFile, serverName)
	if err != nil {
		return nil, errorf("loading daemon CA %s: %w", caFile, err)
	}
	return grpc.WithTransportCredentials(creds), nil
}

// tokenCreds attaches the bearer token to every outgoing call. It requires a
// secure transport, so the token is never sent over an unencrypted channel.
type tokenCreds struct{ token string }

func (t tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + t.token}, nil
}

func (t tokenCreds) RequireTransportSecurity() bool { return true }

// fromStatus maps the daemon's codes.NotFound back to ErrVMNotFound so callers
// that use errors.Is keep working across the transport.
func fromStatus(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) == codes.NotFound {
		return ErrVMNotFound
	}
	return err
}

func (c *grpcClient) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *grpcClient) Exists(ctx context.Context, name string) (bool, error) {
	resp, err := c.rpc.Exists(ctx, &pb.ExistsRequest{Name: name})
	if err != nil {
		return false, fromStatus(err)
	}
	return resp.GetExists(), nil
}

func (c *grpcClient) GetVM(ctx context.Context, name string) (*VM, error) {
	resp, err := c.rpc.GetVM(ctx, &pb.GetVMRequest{Name: name})
	if err != nil {
		return nil, fromStatus(err)
	}
	return vmFromProto(resp.GetVm()), nil
}

func (c *grpcClient) CreateVM(ctx context.Context, in CreateVMInput) (*VM, error) {
	resp, err := c.rpc.CreateVM(ctx, &pb.CreateVMRequest{
		Name:        in.Name,
		MemoryBytes: in.MemoryBytes,
		CpuCount:    int32(in.CPUCount),
		DiskImage:   in.DiskImage,
		Notes:       in.Notes,
	})
	if err != nil {
		return nil, fromStatus(err)
	}
	return vmFromProto(resp.GetVm()), nil
}

func (c *grpcClient) StartVM(ctx context.Context, name string) error {
	_, err := c.rpc.StartVM(ctx, &pb.NameRequest{Name: name})
	return fromStatus(err)
}

func (c *grpcClient) StopVM(ctx context.Context, name string, force bool) error {
	_, err := c.rpc.StopVM(ctx, &pb.StopVMRequest{Name: name, Force: force})
	return fromStatus(err)
}

func (c *grpcClient) RemoveVM(ctx context.Context, name string) error {
	_, err := c.rpc.RemoveVM(ctx, &pb.NameRequest{Name: name})
	return fromStatus(err)
}

func (c *grpcClient) AttachCloudInit(ctx context.Context, name string, userData []byte) error {
	_, err := c.rpc.AttachCloudInit(ctx, &pb.AttachCloudInitRequest{Name: name, UserData: userData})
	return fromStatus(err)
}

func (c *grpcClient) GetIPAddresses(ctx context.Context, name string) ([]string, error) {
	resp, err := c.rpc.GetIPAddresses(ctx, &pb.NameRequest{Name: name})
	if err != nil {
		return nil, fromStatus(err)
	}
	return resp.GetIpAddresses(), nil
}

func (c *grpcClient) ListVMs(ctx context.Context) ([]*VM, error) {
	resp, err := c.rpc.ListVMs(ctx, &pb.Empty{})
	if err != nil {
		return nil, fromStatus(err)
	}
	out := make([]*VM, 0, len(resp.GetVms()))
	for _, v := range resp.GetVms() {
		if vm := vmFromProto(v); vm != nil {
			out = append(out, vm)
		}
	}
	return out, nil
}

// vmFromProto converts a wire VM into the provider's view.
func vmFromProto(v *pb.VM) *VM {
	if v == nil {
		return nil
	}
	return &VM{
		Name:        v.GetName(),
		ID:          v.GetId(),
		State:       v.GetState(),
		CPUCount:    int(v.GetCpuCount()),
		MemoryBytes: v.GetMemoryBytes(),
		DiskImage:   v.GetDiskImage(),
		Notes:       v.GetNotes(),
	}
}
