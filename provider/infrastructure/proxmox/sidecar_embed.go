package proxmox

import _ "embed"

// sweepScript is the deletion sidecar's boot script, baked into the throwaway
// Alpine LXC the sidecar runs as. See sidecar_script.sh and
// LaunchDeletionSidecar.
//
//go:embed sidecar_script.sh
var sweepScript []byte
