#!/bin/sh
# klotho CAPD deletion cleanup.
#
# CAPD's controller runs inside the cluster (node zero). When "klotho delete"
# issues the Cluster API deletion, CAPD removes the worker and control-plane
# containers in order over the host runtime socket -- but it cannot remove the
# LAST control-plane container, because that is the container it is itself
# running in: it deletes that container and dies before the removal completes,
# leaving the last node, the "<cluster>-lb" load balancer and the cluster
# network orphaned.
#
# This script runs in a detached container klotho launches with the host socket
# mounted (docker-outside-of-docker). It waits for Cluster API to delete what it
# can, then force-removes everything the cluster left: the last control-plane
# container, the load balancer and any leftover workers (all carry the
# "io.x-k8s.kind.cluster=<cluster>" label), and finally the cluster network.
#
# The container runs with --rm, so the runtime removes it when this script
# exits. It is NOT labelled with the cluster label, so its own sweep never
# matches it, and it never tries to remove itself over the socket (which the
# runtime cannot complete for a running container).
set -u

: "${CLUSTER:?CLUSTER is required}"
LABEL="io.x-k8s.kind.cluster=${CLUSTER}"
TIMEOUT="${TIMEOUT:-300}"
NETWORK="${NETWORK:-}"

log() { echo "klotho-cleanup[$CLUSTER]: $*"; }

count() {
	docker ps --all --filter "label=${LABEL}" --format '{{.Names}}' 2>/dev/null | grep -c . || true
}

log "waiting for Cluster API to delete the cluster (timeout ${TIMEOUT}s)"
now=$(date +%s)
deadline=$((now + TIMEOUT))
prev=-1
stable=0
while [ "$(date +%s)" -lt "$deadline" ]; do
	n=$(count)
	log "cluster containers remaining: ${n}"
	if [ "$n" -eq 0 ]; then
		break
	fi
	# When the count stops shrinking, Cluster API has removed everything it can
	# and the deadlock (the last control-plane container) has been reached.
	if [ "$n" -eq "$prev" ]; then
		stable=$((stable + 1))
	else
		stable=0
	fi
	if [ "$stable" -ge 3 ]; then
		log "count stable at ${n}; Cluster API cannot remove the rest, finishing it"
		break
	fi
	prev=$n
	sleep 5
done

# Force-remove every container the cluster left behind: the last control-plane
# container (whose controller could not remove it), the load balancer and any
# leftover workers.
for c in $(docker ps --all --filter "label=${LABEL}" --format '{{.Names}}' 2>/dev/null); do
	log "removing container ${c}"
	docker rm --force --volumes "$c" >/dev/null 2>&1 || log "could not remove ${c}"
done

# Remove the cluster's network once nothing is attached. docker/podman refuse to
# remove a network that still has endpoints, so a shared network kept by another
# cluster is left in place.
if [ -n "$NETWORK" ]; then
	case "$NETWORK" in
	podman | bridge | host | none) ;;
	*)
		if docker network rm "$NETWORK" >/dev/null 2>&1; then
			log "removed network ${NETWORK}"
		else
			log "network ${NETWORK} not removed (still in use or already gone)"
		fi
		;;
	esac
fi

log "done"
# The container runs with --rm; exiting removes it.
