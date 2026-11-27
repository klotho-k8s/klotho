package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// runner runs a PowerShell script and returns its stdout. It is an interface
// so the command builders can be unit-tested against a capturing fake.
type runner interface {
	run(ctx context.Context, script string) (stdout string, err error)
}

// powershellRunner runs scripts through powershell.exe on the local host.
type powershellRunner struct{}

func (powershellRunner) run(ctx context.Context, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("powershell: %s", msg)
	}
	return out.String(), nil
}

// psClient implements Client by running Hyper-V cmdlets through a runner.
type psClient struct {
	r runner
}

// NewClient returns a Client backed by local powershell.exe.
func NewClient() Client { return &psClient{r: powershellRunner{}} }

// psQuote renders s as a PowerShell single-quoted literal: wrap in single
// quotes and double any internal single quote. Nothing inside a single-quoted
// literal is expanded, so this is injection-safe for arbitrary values.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// --- command builders (pure functions, table-tested) ---

// getVMScript reads one VM as JSON, emitting nothing when it does not exist.
//
// Get-VM for a missing name still leaves powershell.exe a non-zero exit code
// even under -ErrorAction SilentlyContinue, so the script guards on the result
// and ends with "exit 0": a missing VM is reported as empty output, not as a
// process failure the runner would treat as a hard error.
func getVMScript(name string) string {
	return "$vm = Get-VM -Name " + psQuote(name) + " -ErrorAction SilentlyContinue; " +
		"if ($vm) { $vm | Select-Object Name,Id,State,Notes | ConvertTo-Json -Compress }; exit 0"
}

// listVMsScript reads every VM whose name starts with prefix as a JSON array.
// Like getVMScript it ends with "exit 0" so that no match (an empty result) is
// not reported as a process failure.
func listVMsScript(prefix string) string {
	return "@(Get-VM -Name " + psQuote(prefix+"*") + " -ErrorAction SilentlyContinue | " +
		"Select-Object Name,Id,State,Notes) | ConvertTo-Json -Compress; exit 0"
}

// createVMScript copies the base VHDX to a per-VM disk, creates a Generation 2
// VM from it, sizes it, sets its Notes, and prints the created VM as JSON.
func createVMScript(in CreateInput) string {
	disk := diskPath(in.VMPath, in.Name)
	dir := filepath.ToSlash(filepath.Dir(disk))
	mem := strconv.Itoa(in.MemoryMiB) + "MB"
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	b.WriteString("New-Item -ItemType Directory -Force -Path " + psQuote(dir) + " | Out-Null\n")
	b.WriteString("Copy-Item -LiteralPath " + psQuote(in.Image) + " -Destination " + psQuote(disk) + " -Force\n")
	b.WriteString("New-VM -Name " + psQuote(in.Name) + " -Generation 2 -MemoryStartupBytes " + mem +
		" -VHDPath " + psQuote(disk) + " -SwitchName " + psQuote(in.Switch))
	if in.VMPath != "" {
		b.WriteString(" -Path " + psQuote(in.VMPath))
	}
	b.WriteString(" | Out-Null\n")
	b.WriteString("Set-VMProcessor -VMName " + psQuote(in.Name) + " -Count " + strconv.Itoa(in.CPUs) + "\n")
	b.WriteString("Set-VMMemory -VMName " + psQuote(in.Name) + " -DynamicMemoryEnabled $false\n")
	if in.Notes != "" {
		b.WriteString("Set-VM -Name " + psQuote(in.Name) + " -Notes " + psQuote(in.Notes) + "\n")
	}
	b.WriteString("Get-VM -Name " + psQuote(in.Name) + " | Select-Object Name,Id,State,Notes | ConvertTo-Json -Compress")
	return b.String()
}

// attachISOScript attaches an ISO to the VM as a SCSI DVD drive.
func attachISOScript(name, iso string) string {
	return "Add-VMDvdDrive -VMName " + psQuote(name) + " -Path " + psQuote(iso)
}

// setFirmwareScript sets the Gen2 boot order (hard disk first) and Secure Boot.
func setFirmwareScript(name string, secureBoot bool) string {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	b.WriteString("$d = Get-VMHardDiskDrive -VMName " + psQuote(name) + " | Select-Object -First 1\n")
	b.WriteString("Set-VMFirmware -VMName " + psQuote(name) + " -FirstBootDevice $d")
	if secureBoot {
		b.WriteString(" -EnableSecureBoot On")
	} else {
		b.WriteString(" -EnableSecureBoot Off")
	}
	return b.String()
}

// startVMScript powers the VM on.
func startVMScript(name string) string {
	return "Start-VM -Name " + psQuote(name)
}

// stopVMScript powers the VM off, hard when turnOff is set.
func stopVMScript(name string, turnOff bool) string {
	if turnOff {
		return "Stop-VM -Name " + psQuote(name) + " -TurnOff -Force"
	}
	return "Stop-VM -Name " + psQuote(name) + " -Force"
}

// removeVMScript stops and deletes the VM and its disks; a missing VM is a
// no-op.
//
// A just force-stopped VM's disk stays locked by its worker process (vmwp.exe)
// for a brief window after Remove-VM returns, so deleting the VHDX immediately
// races that handle. A bare "Remove-Item -Force" loses that race as a
// non-terminating error, which the invocation swallows: the VM unregisters but
// its multi-GB disk is left behind as a silent orphan. Retry the delete until
// the handle is released, and throw if it never is, so the orphan surfaces as a
// delete failure instead of silently accumulating on disk.
func removeVMScript(name string) string {
	var b strings.Builder
	b.WriteString("$vm = Get-VM -Name " + psQuote(name) + " -ErrorAction SilentlyContinue\n")
	b.WriteString("if ($vm) {\n")
	b.WriteString("  if ($vm.State -ne 'Off') { Stop-VM -Name " + psQuote(name) + " -TurnOff -Force }\n")
	b.WriteString("  $disks = (Get-VMHardDiskDrive -VMName " + psQuote(name) + ").Path\n")
	b.WriteString("  Remove-VM -Name " + psQuote(name) + " -Force\n")
	b.WriteString("  foreach ($p in $disks) {\n")
	b.WriteString("    if ($p -and (Test-Path -LiteralPath $p)) {\n")
	b.WriteString("      $removed = $false\n")
	b.WriteString("      for ($i = 0; $i -lt 20; $i++) {\n")
	b.WriteString("        try { Remove-Item -LiteralPath $p -Force -ErrorAction Stop; $removed = $true; break }\n")
	b.WriteString("        catch { Start-Sleep -Milliseconds 500 }\n")
	b.WriteString("      }\n")
	b.WriteString("      if (-not $removed) { throw \"failed to remove disk $p: still locked after the VM was stopped\" }\n")
	b.WriteString("    }\n")
	b.WriteString("  }\n")
	b.WriteString("}")
	return b.String()
}

// ipAddressesScript prints the guest IP addresses, one per line. This is the
// KVP path: it only reports addresses when the guest runs the Hyper-V data
// exchange service (hyperv-daemons), which generic cloud images do not.
func ipAddressesScript(name string) string {
	return "(Get-VMNetworkAdapter -VMName " + psQuote(name) + ").IPAddresses"
}

// adapterInfoScript reads the VM's network adapters as JSON, emitting the MAC
// address and the switch each adapter joins. It is the first half of the
// host-side IP fallback: with the MAC and switch known, the guest IP is read
// from the host neighbor table even when the guest reports no KVP address.
// Ends with "exit 0" so a VM with no adapter is empty output, not a failure.
func adapterInfoScript(name string) string {
	return "@(Get-VMNetworkAdapter -VMName " + psQuote(name) + " -ErrorAction SilentlyContinue | " +
		"Select-Object MacAddress,SwitchName) | ConvertTo-Json -Compress; exit 0"
}

// neighborScript reads the host neighbor (ARP) table for one interface as
// JSON, emitting each entry's IP, MAC and reachability state. Querying by the
// switch's host vNIC alias scopes the table to the VM's network. Ends with
// "exit 0" so an alias with no entries (or no such interface) is empty output.
func neighborScript(ifAlias string) string {
	return "@(Get-NetNeighbor -InterfaceAlias " + psQuote(ifAlias) + " -AddressFamily IPv4 -ErrorAction SilentlyContinue | " +
		"Select-Object IPAddress,LinkLayerAddress,State) | ConvertTo-Json -Compress; exit 0"
}

// switchInterfaceAlias is the host virtual NIC alias for a Hyper-V switch: the
// host side of switch "Default Switch" is the interface "vEthernet (Default
// Switch)", whose neighbor table holds the guests' addresses.
func switchInterfaceAlias(switchName string) string {
	return "vEthernet (" + switchName + ")"
}

// listSwitchesScript prints the host's virtual switch names, one per line.
func listSwitchesScript() string {
	return "(Get-VMSwitch).Name"
}

// diskPath is where a VM's copied disk lives: <vmPath>/<name>/<name>.vhdx, or
// just <name>.vhdx in the current dir when vmPath is empty (the client then
// relies on the host default; callers normally set VMPath).
func diskPath(vmPath, name string) string {
	if vmPath == "" {
		return name + ".vhdx"
	}
	return filepath.ToSlash(filepath.Join(vmPath, name, name+".vhdx"))
}

// --- JSON parsing ---

// vmJSON is the shape `Select-Object Name,Id,State,Notes | ConvertTo-Json`
// produces. State may be a string ("Running") or the VMState enum number.
type vmJSON struct {
	Name  string          `json:"Name"`
	ID    string          `json:"Id"`
	State json.RawMessage `json:"State"`
	Notes string          `json:"Notes"`
}

func (j vmJSON) vm() *VM {
	return &VM{Name: j.Name, ID: j.ID, State: normalizeState(j.State), Notes: j.Notes}
}

// normalizeState turns the State field into a word. ConvertTo-Json renders the
// Hyper-V VMState enum as a number in some PowerShell versions and a string in
// others; 2 = Running, 3 = Off.
func normalizeState(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	switch s {
	case "2":
		return StateRunning
	case "3":
		return StateOff
	default:
		return s
	}
}

// --- Client methods ---

func (c *psClient) GetVM(ctx context.Context, name string) (*VM, error) {
	out, err := c.r.run(ctx, getVMScript(name))
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, ErrVMNotFound
	}
	var j vmJSON
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		return nil, fmt.Errorf("hyperv: parsing Get-VM output: %w", err)
	}
	return j.vm(), nil
}

func (c *psClient) CreateVM(ctx context.Context, in CreateInput) (*VM, error) {
	out, err := c.r.run(ctx, createVMScript(in))
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, fmt.Errorf("hyperv: New-VM returned no VM for %q", in.Name)
	}
	var j vmJSON
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		return nil, fmt.Errorf("hyperv: parsing New-VM output: %w", err)
	}
	return j.vm(), nil
}

func (c *psClient) AttachISO(ctx context.Context, name, isoPath string) error {
	_, err := c.r.run(ctx, attachISOScript(name, isoPath))
	return err
}

func (c *psClient) SetFirmware(ctx context.Context, name string, secureBoot bool) error {
	_, err := c.r.run(ctx, setFirmwareScript(name, secureBoot))
	return err
}

func (c *psClient) StartVM(ctx context.Context, name string) error {
	_, err := c.r.run(ctx, startVMScript(name))
	return err
}

func (c *psClient) StopVM(ctx context.Context, name string, turnOff bool) error {
	_, err := c.r.run(ctx, stopVMScript(name, turnOff))
	return err
}

func (c *psClient) RemoveVM(ctx context.Context, name string) error {
	_, err := c.r.run(ctx, removeVMScript(name))
	return err
}

func (c *psClient) ListVMs(ctx context.Context, prefix string) ([]*VM, error) {
	out, err := c.r.run(ctx, listVMsScript(prefix))
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return nil, nil
	}
	// ConvertTo-Json renders a single element as an object, many as an array.
	var many []vmJSON
	if err := json.Unmarshal([]byte(out), &many); err != nil {
		var one vmJSON
		if err2 := json.Unmarshal([]byte(out), &one); err2 != nil {
			return nil, fmt.Errorf("hyperv: parsing Get-VM list output: %w", err)
		}
		many = []vmJSON{one}
	}
	vms := make([]*VM, 0, len(many))
	for _, j := range many {
		vms = append(vms, j.vm())
	}
	return vms, nil
}

func (c *psClient) GetIPAddresses(ctx context.Context, name string) ([]string, error) {
	out, err := c.r.run(ctx, ipAddressesScript(name))
	if err != nil {
		return nil, err
	}
	addrs := lines(out)
	// KVP reported an IPv4 address: the guest runs hyperv-daemons, trust it.
	if len(ipv4(addrs)) > 0 {
		return addrs, nil
	}
	// No KVP IPv4 (the generic cloud image has no hyperv-daemons): fall back to
	// the host neighbor table. A fallback failure is not fatal to the caller,
	// which keeps polling; return whatever KVP gave (possibly empty).
	fb, ferr := c.neighborIPv4(ctx, name)
	if ferr != nil {
		return addrs, nil
	}
	return append(addrs, fb...), nil
}

// neighborIPv4 resolves the VM's IPv4 address from the host neighbor table when
// the guest reports none over KVP. It reads the VM adapters' MAC and switch,
// then reads the neighbor table of each switch's host vNIC and returns the
// IPv4 addresses whose link-layer address matches a VM MAC, preferring
// Reachable over Stale entries. An empty result is valid (the guest may not
// have sent traffic yet), so it returns no error for "nothing found".
func (c *psClient) neighborIPv4(ctx context.Context, name string) ([]string, error) {
	out, err := c.r.run(ctx, adapterInfoScript(name))
	if err != nil {
		return nil, err
	}
	adapters, err := parseAdapters(out)
	if err != nil {
		return nil, err
	}
	macs := map[string]bool{}
	switches := map[string]bool{}
	for _, a := range adapters {
		if m := normalizeMAC(a.MacAddress); m != "" {
			macs[m] = true
		}
		if a.SwitchName != "" {
			switches[a.SwitchName] = true
		}
	}
	if len(macs) == 0 || len(switches) == 0 {
		return nil, nil
	}
	var entries []neighbor
	for sw := range switches {
		nout, nerr := c.r.run(ctx, neighborScript(switchInterfaceAlias(sw)))
		if nerr != nil {
			continue
		}
		es, perr := parseNeighbors(nout)
		if perr != nil {
			continue
		}
		entries = append(entries, es...)
	}
	return pickNeighborIPv4(entries, macs), nil
}

// normalizeMAC strips the separators Hyper-V and Get-NetNeighbor disagree on
// (Get-VMNetworkAdapter reports "00155D0A1B2C", Get-NetNeighbor reports
// "00-15-5D-0A-1B-2C") and upper-cases, so the two can be compared.
func normalizeMAC(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '-', ':', '.', ' ':
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}

// adapterInfo is the shape adapterInfoScript emits per VM network adapter.
type adapterInfo struct {
	MacAddress string `json:"MacAddress"`
	SwitchName string `json:"SwitchName"`
}

// parseAdapters decodes adapterInfoScript output, which ConvertTo-Json renders
// as a single object for one adapter and an array for several.
func parseAdapters(out string) ([]adapterInfo, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return nil, nil
	}
	var many []adapterInfo
	if err := json.Unmarshal([]byte(out), &many); err != nil {
		var one adapterInfo
		if err2 := json.Unmarshal([]byte(out), &one); err2 != nil {
			return nil, fmt.Errorf("hyperv: parsing adapter output: %w", err)
		}
		return []adapterInfo{one}, nil
	}
	return many, nil
}

// neighbor is one host neighbor-table entry.
type neighbor struct {
	IPAddress        string          `json:"IPAddress"`
	LinkLayerAddress string          `json:"LinkLayerAddress"`
	State            json.RawMessage `json:"State"`
}

// parseNeighbors decodes neighborScript output (object for one entry, array for
// several).
func parseNeighbors(out string) ([]neighbor, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return nil, nil
	}
	var many []neighbor
	if err := json.Unmarshal([]byte(out), &many); err != nil {
		var one neighbor
		if err2 := json.Unmarshal([]byte(out), &one); err2 != nil {
			return nil, fmt.Errorf("hyperv: parsing neighbor output: %w", err)
		}
		return []neighbor{one}, nil
	}
	return many, nil
}

// neighborState turns the State field into a word. Get-NetNeighbor renders the
// NlNeighborState enum as a number in some PowerShell versions and a string in
// others. The values that matter: 2 = Stale, 3 = Delay, 4 = Probe, 5 = Reachable
// (Permanent is reported as its word).
func neighborState(raw json.RawMessage) string {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	switch s {
	case "5":
		return "Reachable"
	case "2":
		return "Stale"
	case "0":
		return "Unreachable"
	case "1":
		return "Incomplete"
	case "6":
		return "Permanent"
	default:
		return s
	}
}

// neighborRank orders states by how much they are trusted as a current address:
// Reachable first, then Permanent/Stale, then the transient probing states.
// Unreachable and Incomplete are dropped (rank < 0).
func neighborRank(state string) int {
	switch state {
	case "Reachable":
		return 0
	case "Permanent":
		return 1
	case "Stale":
		return 2
	case "Delay", "Probe":
		return 3
	default: // Unreachable, Incomplete, unknown
		return -1
	}
}

// pickNeighborIPv4 returns the IPv4 addresses of the neighbor entries whose MAC
// is in macs, ordered best-state first (Reachable before Stale), de-duplicated.
func pickNeighborIPv4(entries []neighbor, macs map[string]bool) []string {
	type cand struct {
		ip   string
		rank int
	}
	var cands []cand
	for _, e := range entries {
		if !macs[normalizeMAC(e.LinkLayerAddress)] {
			continue
		}
		if ip := net.ParseIP(e.IPAddress); ip == nil || ip.To4() == nil {
			continue
		}
		rank := neighborRank(neighborState(e.State))
		if rank < 0 {
			continue
		}
		cands = append(cands, cand{ip: e.IPAddress, rank: rank})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rank < cands[j].rank })
	var out []string
	seen := map[string]bool{}
	for _, c := range cands {
		if seen[c.ip] {
			continue
		}
		seen[c.ip] = true
		out = append(out, c.ip)
	}
	return out
}

func (c *psClient) ListSwitches(ctx context.Context) ([]string, error) {
	out, err := c.r.run(ctx, listSwitchesScript())
	if err != nil {
		return nil, err
	}
	return lines(out), nil
}

// lines splits output into non-empty trimmed lines.
func lines(out string) []string {
	var res []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			res = append(res, s)
		}
	}
	return res
}
