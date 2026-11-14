// Package chclient is a Go client for the subset of Cloud Hypervisor's REST
// VMM API needed to hot-attach/detach a real VFIO (GPU) device to/from an
// already-running guest: /api/v1/vmm.ping (readiness/liveness) and the
// device hotplug pair, /api/v1/vm.add-device and /api/v1/vm.remove-device.
//
// This is new, independent code -- it does not import Agent Substrate's own
// internal/ch package (a different Go module's internal package, not
// importable anyway) -- but it deliberately mirrors that package's transport
// approach (HTTP/1.1 over the VMM's unix api-socket, one fresh connection per
// request) since that choice is already proven correct against a real
// cloud-hypervisor instance there. See research/live-attach-poc/README.md
// for why this exists: Substrate's microVM worker (ateworker-microvm) runs
// Kata on cloud-hypervisor, and cloud-hypervisor's hotplug API is the one
// concrete, already-shipped mechanism found that can attach a device to an
// actor's sandbox without recreating it.
//
// Request/response shapes here were taken directly from cloud-hypervisor's
// own OpenAPI spec (vmm/src/api/openapi/cloud-hypervisor.yaml, DeviceConfig /
// VmRemoveDevice / PciDeviceInfo) and docs/hotplug.md, not guessed.
package chclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// apiBase is a placeholder host; the real transport always dials the unix
// api-socket, so the host portion of the URL is ignored.
const apiBase = "http://localhost"

// Client talks to one cloud-hypervisor VMM over its unix api-socket (e.g.
// the socket kata creates at /run/vc/vm/<id>/clh-api.sock).
type Client struct {
	http *http.Client
}

// NewClient returns a Client bound to a cloud-hypervisor api-socket path.
// The socket need not exist yet; use WaitReady to block until the VMM
// answers.
func NewClient(socketPath string) *Client {
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{
				// cloud-hypervisor's API server closes idle connections;
				// force a fresh connection per request rather than risk
				// reusing one that's already been closed server-side
				// (the same workaround Substrate's own ch package applies,
				// for the same observed symptom).
				DisableKeepAlives: true,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// VMMInfo is what vmm.ping reports about the running VMM.
type VMMInfo struct {
	Version      string   `json:"version"`
	BuildVersion string   `json:"build_version"`
	Features     []string `json:"features"`
}

// Ping reports what the VMM says about itself, or an error if the
// api-socket does not answer vmm.ping.
func (c *Client) Ping(ctx context.Context) (VMMInfo, error) {
	var info VMMInfo
	err := c.getJSON(ctx, "/api/v1/vmm.ping", &info)
	return info, err
}

// WaitReady blocks until the api-socket answers vmm.ping or the deadline
// passes.
func (c *Client) WaitReady(ctx context.Context, deadline time.Duration) (VMMInfo, error) {
	end := time.Now().Add(deadline)
	for {
		info, err := c.Ping(ctx)
		if err == nil {
			return info, nil
		}
		if !time.Now().Before(end) {
			return VMMInfo{}, fmt.Errorf("cloud-hypervisor api socket not ready after %s: %w", deadline, err)
		}
		select {
		case <-ctx.Done():
			return VMMInfo{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// DeviceConfig is the request body of PUT /api/v1/vm.add-device. Path is the
// VFIO device's sysfs path (e.g. "/sys/bus/pci/devices/0000:01:00.0/"); ID,
// if set, is the identifier used later to remove it -- leave it empty to let
// cloud-hypervisor assign one, but then its own response (or a subsequent
// vm.info) is the only way to learn what it picked, so callers that need to
// remove the device later should normally set ID themselves. Iommu is
// whether the device sits behind a virtual IOMMU -- a plain JSON boolean in
// cloud-hypervisor's real vm-config::DeviceConfig, not the string enum this
// field used to be typed as. Confirmed the hard way (2026-11-13): sending
// {"iommu":"Off"} got a real 400 back from a live cloud-hypervisor VMM --
// "invalid type: string \"Off\", expected a boolean" -- once every earlier
// dial/socket-path bug blocking AddDevice from ever reaching the VMM at all
// was fixed. False (a plain VFIO passthrough device, no virtual IOMMU) is
// cloud-hypervisor's own default, so omitempty dropping it is equivalent to
// sending it explicitly.
type DeviceConfig struct {
	Path  string `json:"path,omitempty"`
	Iommu bool   `json:"iommu,omitempty"`
	ID    string `json:"id,omitempty"`
}

// PciDeviceInfo is the 200-OK response body of a hot (VM already running)
// vm.add-device call: BDF is the device's address inside the guest (e.g.
// "0000:00:08.0"), distinct from the host-side BDF in DeviceConfig.Path.
type PciDeviceInfo struct {
	ID  string `json:"id"`
	BDF string `json:"bdf"`
}

// AddDevice hot-attaches a VFIO device (identified by its host sysfs path)
// to the running guest. On a hot add (VM already running), cloud-hypervisor
// returns 200 with a PciDeviceInfo naming the guest-side BDF; on a cold add
// (VM not yet booted) it returns 204 with no body, in which case the
// returned PciDeviceInfo is zero and BDF cannot be known until the guest
// boots and the guest OS enumerates its PCI bus.
func (c *Client) AddDevice(ctx context.Context, cfg DeviceConfig) (PciDeviceInfo, error) {
	var info PciDeviceInfo
	hadBody, err := c.putJSON(ctx, "/api/v1/vm.add-device", cfg, &info)
	if err != nil {
		if bdf, ok := c.alreadyAttached(ctx, cfg.ID, err); ok {
			return PciDeviceInfo{ID: cfg.ID, BDF: bdf}, nil
		}
		return PciDeviceInfo{}, err
	}
	if !hadBody {
		return PciDeviceInfo{}, nil
	}
	return info, nil
}

// alreadyAttached reports whether addErr is cloud-hypervisor's own
// "Duplicated device path" validation error for id, and if the device is
// genuinely already present (confirmed via vm.info, not trusted from the
// error text alone), returns its real guest BDF. Confirmed live
// (2026-11-14), two different ways: a still-running VM whose
// vm.remove-device had reported success without cloud-hypervisor's device
// manager actually finishing the removal yet (see RemoveDevice), and --
// found while reproducing that same error after the fix above -- a
// freshly-resumed VM that Agent Substrate's own restore-from-checkpoint
// path booted with this device already baked into its config (confirmed
// via vm.info showing it present, with a real device_tree PciBdf, on a VM
// AddDevice had never actually been called against successfully). Either
// way, a hard failure here would make the device permanently unattachable
// for this actor; treating the state cloud-hypervisor itself reports as
// the success it already is lets the reconciler record the right BDF
// instead.
func (c *Client) alreadyAttached(ctx context.Context, id string, addErr error) (bdf string, ok bool) {
	if !strings.Contains(addErr.Error(), "Duplicated device path") {
		return "", false
	}
	info, err := c.Info(ctx)
	if err != nil {
		return "", false
	}
	found := false
	for _, d := range info.Config.Devices {
		if d.ID == id {
			found = true
			break
		}
	}
	if !found {
		return "", false
	}
	return info.DeviceTree[id].PciBdf, true
}

// VMConfig is the "config" field of GET /api/v1/vm.info's response. Only
// Devices is modeled; cloud-hypervisor's real config carries much more
// (cpus, memory, net, fs, ...), and encoding/json silently ignores the
// rest.
type VMConfig struct {
	Devices []DeviceConfig `json:"devices"`
}

// DeviceTreeNode is one entry of GET /api/v1/vm.info's "device_tree" map,
// keyed by device ID. Only PciBdf is modeled; confirmed against a real
// cloud-hypervisor instance's response (2026-11-14) to carry the device's
// real guest-side PCI address once it's actually plugged in.
type DeviceTreeNode struct {
	PciBdf string `json:"pci_bdf"`
}

// VMInfo is the response body of GET /api/v1/vm.info.
type VMInfo struct {
	Config     VMConfig                  `json:"config"`
	DeviceTree map[string]DeviceTreeNode `json:"device_tree"`
}

// Info reports the VMM's current view of the VM, including which VFIO
// devices its config still lists -- confirmed live (2026-11-14) as the
// authoritative way to tell whether a device is *actually* gone, as
// opposed to RemoveDevice's own HTTP response alone (see its doc comment).
func (c *Client) Info(ctx context.Context) (VMInfo, error) {
	var info VMInfo
	err := c.getJSON(ctx, "/api/v1/vm.info", &info)
	return info, err
}

// removeConfirmRetries/removeConfirmDelay bound how long RemoveDevice polls
// vm.info to confirm a device is actually gone after a successful
// vm.remove-device call -- see RemoveDevice's doc comment for why this
// exists. 40 * 50ms = 2s, comfortably above every detach latency observed
// live so far (124ms, 854ms).
const (
	removeConfirmRetries = 40
	removeConfirmDelay   = 50 * time.Millisecond
)

// RemoveDevice hot-detaches the device previously added under id (the ID
// given to AddDevice, or the one it returned if none was given).
//
// Confirmed live (2026-11-14): a successful (200) vm.remove-device response
// does not reliably mean the device is actually gone from cloud-hypervisor's
// own bookkeeping yet -- a real repeated attach/detach/attach cycle hit
// "Duplicated device path" on the very next vm.add-device against the same
// still-running VM, immediately after vm.remove-device had already reported
// success for that exact path. Cloud-hypervisor's own device manager
// apparently completes some of a hot-remove's internal cleanup
// asynchronously (plausibly the guest-side ACPI unplug handshake) after its
// HTTP response returns. RemoveDevice now polls vm.info after the PUT
// succeeds and only returns nil once id no longer appears in
// VMInfo.Config.Devices, so a caller that got a nil error can trust the
// device is genuinely free to re-add -- rather than trusting the PUT's own
// status code, which this finding shows is not sufficient on its own.
func (c *Client) RemoveDevice(ctx context.Context, id string) error {
	if _, err := c.putJSON(ctx, "/api/v1/vm.remove-device", struct {
		ID string `json:"id"`
	}{ID: id}, nil); err != nil {
		return err
	}

	for attempt := 0; ; attempt++ {
		info, err := c.Info(ctx)
		if err != nil {
			return fmt.Errorf("confirming device %q was removed: %w", id, err)
		}
		stillPresent := false
		for _, d := range info.Config.Devices {
			if d.ID == id {
				stillPresent = true
				break
			}
		}
		if !stillPresent {
			return nil
		}
		if attempt == removeConfirmRetries {
			return fmt.Errorf("device %q still present in vm.info's config.devices %v after vm.remove-device reported success (waited %s)",
				id, info.Config.Devices, time.Duration(removeConfirmRetries)*removeConfirmDelay)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(removeConfirmDelay):
		}
	}
}

// dialRetries/dialRetryDelay bound how many times a request is retried
// when it fails at the dial step itself (before any bytes reach the VMM).
// Confirmed live (2026-11-13): this exact client's dial against a real,
// continuously up cloud-hypervisor api-socket intermittently fails with
// "connect: no such file or directory" -- a measured success rate as low
// as ~1-in-8 across repeated back-to-back attempts -- even though the
// identical socket, at the same instant, was reliably reachable from
// independent processes (curl --unix-socket, a standalone binary built on
// this same package) and a preceding successful call on the very same
// Client did not reliably prevent the next one from hitting it either. The
// underlying cause was not pinned down (the socket's existence,
// permissions, and the VMM behind it were all independently verified
// stable throughout), so this retries the dial itself a few times, cheaply
// and only when the failure is dial-level, rather than masking it with a
// guess about the root cause. It never retries after a request has
// actually reached the VMM (a real HTTP response, even an error one, is
// returned as-is) -- vm.add-device's retried callers must not risk
// invoking its real, possibly-mutating request body twice.
const (
	dialRetries    = 8
	dialRetryDelay = 15 * time.Millisecond
)

// isDialFailure reports whether err is a failure to establish the
// connection itself, as opposed to a timeout/cancellation or any error
// that implies a request actually reached the far side.
func isDialFailure(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// doWithDialRetry issues the request built by newReq, retrying (rebuilding
// a fresh request each time) only while the failure is dial-level -- see
// dialRetries.
func (c *Client) doWithDialRetry(newReq func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err == nil {
			return resp, nil
		}
		if !isDialFailure(err) || attempt == dialRetries {
			return nil, err
		}
		time.Sleep(dialRetryDelay)
	}
}

// getJSON issues a GET and decodes the 2xx JSON response into out.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.doWithDialRetry(func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s: status %d: %s", path, resp.StatusCode, bytes.TrimSpace(b))
	}
	return json.Unmarshal(b, out)
}

// putJSON issues a PUT with a JSON body and, on a 200 with a non-empty body,
// decodes it into out (hadBody reports whether that happened; a 204 or an
// empty 200 body leaves out untouched and returns hadBody=false). out may be
// nil if the caller doesn't need the response body at all.
func (c *Client) putJSON(ctx context.Context, path string, body, out any) (hadBody bool, err error) {
	b, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	resp, err := c.doWithDialRetry(func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, apiBase+path, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("PUT %s: status %d: %s", path, resp.StatusCode, bytes.TrimSpace(respBody))
	}
	if out == nil || len(bytes.TrimSpace(respBody)) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return false, fmt.Errorf("PUT %s: decoding response: %w", path, err)
	}
	return true, nil
}
