# Testing the klotho vz provider

The provider is fully unit-tested against a fake capvz-daemon client (`go test
./provider/infrastructure/vz/...`), with no Mac and no running daemon required.
This document is the live-run runbook for a real create -> pivot -> delete on an
**Apple-Silicon Mac**, which is the one thing that cannot be exercised on the
Linux/Windows dev box: Virtualization.framework is cgo-bound, main-thread affine,
and macOS-only.

klotho reaches the host-side `capvz-daemon` over gRPC (TLS + bearer token), the
same transport the in-cluster Cluster API vz controller uses. Because the daemon
runs on the host, not inside the cluster, there is **no deletion sidecar**:
klotho's own `Teardown` force-stops and removes the VMs through the daemon.

## What the provider does on a live run

1. **Prepare** resolves and validates the base image path, the control plane
   endpoint (`CONTROL_PLANE_ENDPOINT_HOST` — vz has no load balancer, so the
   endpoint is the single control-plane node's own NAT address), and that the
   daemon endpoint (`vz.endpoint`) is set. It allocates nothing and injects
   `VZ_HOST`, `VZ_IMAGE_PATH` and `CONTROL_PLANE_ENDPOINT_HOST` into generation.
2. **Launch** registers `<cluster>-bootstrap` with the daemon (which copies the
   raw arm64 base image and assigns a `vmid`), sends the bootstrap user-data over
   `AttachCloudInit` (the daemon builds the NoCloud `cidata` seed on the Mac with
   `instance-id` = the `vmid`), starts the VM's runner, and waits for an IPv4
   address (the daemon reads it from the host DHCP leases, matched by the VM's
   MAC). The node's provider ID is `vz://<host>/<vmid>`.
3. **Adopt** renders the `VZMachine` with `spec.vmName = <cluster>-bootstrap` so
   the provider adopts the running VM by name. It does **not** pre-set
   `spec.providerID`; the controller sets it, and because the kubelet
   `--provider-id` is `vz://<host>/{{ ds.meta_data.instance_id }}` and the
   instance-id is the `vmid`, the IDs agree and the Machine<->Node nodeRef
   completes.
4. **Teardown** force-stops and removes each `<cluster>-*` VM that carries the
   `klotho-<cluster>` tag in its notes, deleting the per-VM working disk and seed
   with it. **The base image is never touched.**

## Prerequisites

- An **Apple-Silicon Mac** (M-series) with **Xcode command line tools**
  (`xcode-select --install`) and **Go 1.26+**.
- A **raw arm64 Linux cloud image** on the Mac (the path you set as `vz.image` /
  `VZ_IMAGE_PATH`). Convert a qcow2 cloud image with
  `qemu-img convert -O raw in.qcow2 out.raw`. If it carries no Kubernetes, create
  the cluster with `--install-kubernetes` so containerd and kubeadm/kubelet/
  kubectl are installed at boot.
- A management cluster to pivot from (kind/Lima on the same Mac is simplest).

## 1. Build, sign, and run the daemon on the Mac

From a clone of
[cluster-api-provider-vz](https://github.com/klotho-k8s/cluster-api-provider-vz):

```sh
# Build the host binaries (the runner links Virtualization.framework via cgo).
make build-daemon        # bin/capvz-daemon
make build-runner        # bin/capvz-runner   (cgo; macOS only)

# Codesign the runner ONCE with the virtualization entitlement. All VMs use this
# same signed binary. (The daemon is signed too for good measure.)
make sign                # codesign --entitlements entitlements.plist -s - bin/capvz-runner

# Install the binaries and a token.
sudo mkdir -p /opt/capvz/data
sudo cp bin/capvz-daemon bin/capvz-runner /opt/capvz/
printf '%s' "$(openssl rand -hex 32)" | sudo tee /opt/capvz/data/token >/dev/null
sudo chmod 600 /opt/capvz/data/token
```

Run the daemon as a launchd LaunchAgent, or in the foreground while iterating:

```sh
# LaunchAgent (survives logout, re-adopts runners on restart):
cp launchd/io.klotho.capvz.daemon.plist ~/Library/LaunchAgents/
#   edit paths/token in the plist if you installed elsewhere, then:
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.klotho.capvz.daemon.plist

# …or foreground:
VZ_DAEMON_TOKEN=$(cat /opt/capvz/data/token) \
  ./bin/capvz-daemon run --data-dir ./data --runner-bin ./bin/capvz-runner \
  --listen 0.0.0.0:7443
```

On first run with no `--tls-cert`/`--tls-key` the daemon generates a self-signed
certificate into its data dir covering `localhost`, the bound host IPs, the
hostname, and any `--tls-hosts`. Copy that cert to the box running klotho and
point `vz.ca` at it (or use a CA you provided). If klotho runs on the same Mac,
`localhost:7443` with the generated cert works out of the box.

## 2. Point klotho at the daemon

In `~/.config/klotho/config.yaml` (or via env / flags):

```yaml
vz:
  endpoint: localhost:7443          # the capvz-daemon gRPC address
  ca: /opt/capvz/data/cert.pem      # the daemon's self-signed cert, or omit for system roots
  host: localhost                   # recorded in providerID vz://<host>/<vmid>
  image: /opt/images/ubuntu-24.04-arm64.raw
  endpointHost: 192.168.64.10       # node zero's address on the vz NAT network
```

Supply the token out of band (never store it in the file):

```sh
export KLOTHO_VZ_TOKEN=$(cat /opt/capvz/data/token)
```

The same endpoint and token are handed to the in-cluster controller as
`VZ_DAEMON_ENDPOINT` / `VZ_DAEMON_TOKEN` (a Secret in `capi-vz-system`) during
pivot, so the controller reaches the same daemon klotho dialed.

## 3. Create, pivot, delete

```sh
klotho create --provider vz --name lab1 --install-kubernetes
# watch node zero boot (the daemon's log shows the runner starting and the seed
# being built), the CAPI stack come up, then the pivot to self-managed.

kubectl --kubeconfig lab1.kubeconfig get machines,vzmachines -A
#   the VZMachine should be Ready with status.adopted=true and
#   spec.providerID = vz://localhost/<vmid>

klotho delete --name lab1
#   klotho's Teardown force-stops and removes <cluster>-* VMs through the daemon;
#   confirm with `launchctl list | grep io.klotho.capvz` (no runners left) and the
#   daemon's registry/data dir (no vms/<vmid> dirs for the cluster).
```

## The three things we care about

1. Does the cluster come up (node zero → CAPI stack Ready)?
2. Does it **pivot to self-managed** (the in-cluster controller reaches the
   daemon with the shared token and keeps the VZMachine reconciled)?
3. Does `klotho delete` reach **zero** — no leftover runners
   (`launchctl list`), no `vms/<vmid>` working dirs, base image untouched?

## Troubleshooting

- **TLS handshake / certificate errors:** the daemon's self-signed cert must
  cover the name in `vz.endpoint`. Dialing by IP a cert issued for a hostname?
  Set `vz.serverName` to the hostname, or add the IP to `--tls-hosts` when the
  daemon generates its cert.
- **`Unauthenticated`:** the `KLOTHO_VZ_TOKEN` klotho sends must byte-match the
  daemon's `/opt/capvz/data/token`.
- **No IPv4 address (Launch times out):** the guest has not requested DHCP yet,
  or the base image is not cloud-init enabled. Check the daemon log and the host
  DHCP leases (`/var/db/dhcpd_leases`); confirm the image boots via EFI and runs
  cloud-init against the `CIDATA` seed.
- **Runner did not start:** confirm `capvz-runner` is codesigned with the
  virtualization entitlement (`codesign -d --entitlements - bin/capvz-runner`).
