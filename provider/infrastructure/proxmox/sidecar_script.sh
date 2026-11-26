#!/bin/sh
# klotho deletion sidecar.
#
# Baked into a throwaway Alpine LXC and run once at boot by the container's
# init. It finishes a cluster's teardown after klotho has exited: it waits for
# Cluster API to delete what it can, destroys the control plane VM the cluster
# could not remove while running on it, frees the bootstrap ISO, deletes its own
# one-shot template, and finally destroys its own container. All of it is done
# over the Proxmox API with the credential baked into the env file; nothing here
# needs the host or root on the host.
set -u

ENV_FILE=/etc/klotho/sweep.env
[ -r "$ENV_FILE" ] && . "$ENV_FILE"

# Baked binaries (curl, jq) live in /usr/local/bin; /sbin has ip/udhcpc. Set an
# explicit PATH because BusyBox init starts this with a minimal one.
export PATH=/usr/local/bin:/usr/sbin:/sbin:/usr/bin:/bin

log() { echo "[klotho-sweep] $*"; }

: "${KLOTHO_POLL_SECONDS:=15}"
: "${KLOTHO_MAX_WAIT_SECONDS:=1800}"
: "${KLOTHO_PVE_INSECURE:=0}"

# Bring eth0 up. This environment has no DHCP, so klotho bakes a static address
# (KLOTHO_NET_*) derived from the cluster's network config; the custom rootfs is
# not one Proxmox configures, so configure the interface here. Fall back to a
# DHCP lease when no static address was baked.
if [ -n "${KLOTHO_NET_IP:-}" ]; then
	: "${KLOTHO_NET_PREFIX:=24}"
	log "configuring eth0 ${KLOTHO_NET_IP}/${KLOTHO_NET_PREFIX} gw ${KLOTHO_NET_GW:-none} dns ${KLOTHO_NET_DNS:-none}"
	ip link set eth0 up 2>/dev/null
	ip addr add "${KLOTHO_NET_IP}/${KLOTHO_NET_PREFIX}" dev eth0 2>/dev/null
	[ -n "${KLOTHO_NET_GW:-}" ] && ip route add default via "$KLOTHO_NET_GW" 2>/dev/null
	[ -n "${KLOTHO_NET_DNS:-}" ] && printf 'nameserver %s\n' "$KLOTHO_NET_DNS" > /etc/resolv.conf
else
	log "no static address baked; bringing eth0 up with DHCP"
	ip link set eth0 up 2>/dev/null
	udhcpc -i eth0 -n -q >/dev/null 2>&1 || true
fi

CURL_OPTS="-sS --connect-timeout 10 --max-time 60"
[ "$KLOTHO_PVE_INSECURE" = "1" ] && CURL_OPTS="$CURL_OPTS -k"
AUTH="Authorization: PVEAPIToken=${KLOTHO_PVE_TOKEN}=${KLOTHO_PVE_SECRET}"
API="${KLOTHO_PVE_URL%/}"
case "$API" in
	*/api2/json) ;;
	*) API="$API/api2/json" ;;
esac

# api METHOD PATH [curl args...] -> prints the response body.
api() {
	method=$1; path=$2; shift 2
	curl $CURL_OPTS -X "$method" -H "$AUTH" "$@" "$API$path"
}

# curl and jq are baked into the image statically linked, so the sweep needs no
# package install and no internet, only LAN reachability to the Proxmox API.
if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
	log "curl/jq missing from the image; giving up (the cluster's VMs may need a manual sweep)"
	exit 1
fi

# tagged_vms prints "node vmid" for every running qemu VM carrying the cluster
# tag (templates and containers excluded).
tagged_vms() {
	api GET "/cluster/resources?type=vm" \
		| jq -r --arg tag "$KLOTHO_CLUSTER_TAG" '
			.data[]
			| select(.type=="qemu")
			| select((.template // 0) != 1)
			| select(((.tags // "") | ascii_downcase | split(";")) | index($tag | ascii_downcase))
			| "\(.node) \(.vmid)"'
}

# Wait for Cluster API to delete what it can: poll until the set of tagged VMs
# stops shrinking (it settles on the last control plane VM, which cannot delete
# itself) or the absolute cap is reached.
log "waiting for Cluster API to delete cluster tagged $KLOTHO_CLUSTER_TAG"
deadline=$(( $(date +%s) + KLOTHO_MAX_WAIT_SECONDS ))
prev=-1
stable=0
while :; do
	count=$(tagged_vms | wc -l | tr -d ' ')
	now=$(date +%s)
	[ "$count" -eq 0 ] && break
	if [ "$count" -eq "$prev" ]; then
		stable=$((stable + 1))
	else
		stable=0
	fi
	[ "$stable" -ge 3 ] && break
	[ "$now" -ge "$deadline" ] && { log "timed out waiting; sweeping the $count VM(s) that remain"; break; }
	prev=$count
	sleep "$KLOTHO_POLL_SECONDS"
done

# Destroy whatever is left: stop (not shutdown) then purge-delete.
tagged_vms | while read -r node vmid; do
	[ -n "$node" ] || continue
	log "destroying VM $vmid on $node"
	api POST "/nodes/$node/qemu/$vmid/status/stop" >/dev/null 2>&1
	# Give the stop task a moment; the destroy fails against a running VM.
	j=0
	while [ "$j" -lt 24 ]; do
		st=$(api GET "/nodes/$node/qemu/$vmid/status/current" | jq -r '.data.status // "unknown"' 2>/dev/null)
		[ "$st" = "stopped" ] && break
		[ "$st" = "unknown" ] && break
		j=$((j + 1))
		sleep 5
	done
	api DELETE "/nodes/$node/qemu/$vmid?purge=1" >/dev/null 2>&1
done

# Free the bootstrap ISO(s): entries are "node|storage:iso/file".
for entry in $KLOTHO_ISO_VOLIDS; do
	node=${entry%%|*}
	volid=${entry#*|}
	[ -n "$node" ] && [ -n "$volid" ] || continue
	log "freeing ISO $volid on $node"
	api DELETE "/nodes/$node/storage/${volid%%:*}/content/$volid" >/dev/null 2>&1
done

# Delete this sidecar's own one-shot template: "node|storage:vztmpl/file".
if [ -n "${KLOTHO_TEMPLATE_VOLID:-}" ]; then
	node=${KLOTHO_TEMPLATE_VOLID%%|*}
	volid=${KLOTHO_TEMPLATE_VOLID#*|}
	if [ -n "$node" ] && [ -n "$volid" ]; then
		log "deleting own template $volid on $node"
		api DELETE "/nodes/$node/storage/${volid%%:*}/content/$volid" >/dev/null 2>&1
	fi
fi

# Finally, destroy this container. force stops it mid-request; this process is
# killed when that lands, which is the intended end of the sidecar's life.
log "deleting self (container $KLOTHO_SELF_VMID on $KLOTHO_SELF_NODE)"
api DELETE "/nodes/$KLOTHO_SELF_NODE/lxc/$KLOTHO_SELF_VMID?purge=1&force=1" >/dev/null 2>&1 &
sleep 30
# If the container is somehow still here, stop looping init.
poweroff -f 2>/dev/null || halt -f 2>/dev/null || true
