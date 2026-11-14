package chclient

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// newTestServer starts a real HTTP server listening on a unix socket (not
// TCP), matching exactly how cloud-hypervisor itself serves its api-socket,
// so these tests exercise the real unix-socket transport path in Client,
// not just its JSON (de)serialization.
func newTestServer(t *testing.T, mux *http.ServeMux) (socketPath string) {
	t.Helper()
	dir := t.TempDir()
	socketPath = filepath.Join(dir, "clh-api.sock")

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listening on unix socket: %v", err)
	}

	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: mux}}
	srv.Start()
	t.Cleanup(srv.Close)
	return socketPath
}

func TestPing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vmm.ping", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(VMMInfo{Version: "53.0.0", BuildVersion: "v53.0"})
	})
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	info, err := c.Ping(context.Background())
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if info.Version != "53.0.0" {
		t.Errorf("Version = %q, want %q", info.Version, "53.0.0")
	}
}

func TestWaitReady_SucceedsOnceSocketAnswers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vmm.ping", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(VMMInfo{Version: "53.0.0"})
	})
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	info, err := c.WaitReady(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if info.Version != "53.0.0" {
		t.Errorf("Version = %q, want %q", info.Version, "53.0.0")
	}
}

func TestWaitReady_TimesOutIfSocketNeverAnswers(t *testing.T) {
	// A socket path that nothing is listening on.
	c := NewClient(filepath.Join(t.TempDir(), "nope.sock"))
	_, err := c.WaitReady(context.Background(), 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
}

func TestAddDevice_HotAdd_Returns200WithPciDeviceInfo(t *testing.T) {
	mux := http.NewServeMux()
	var gotBody DeviceConfig
	mux.HandleFunc("/api/v1/vm.add-device", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(PciDeviceInfo{ID: "gnwes-gpu-0", BDF: "0000:00:08.0"})
	})
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	cfg := DeviceConfig{Path: "/sys/bus/pci/devices/0000:01:00.0/", ID: "gnwes-gpu-0"}
	info, err := c.AddDevice(context.Background(), cfg)
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	if info.BDF != "0000:00:08.0" {
		t.Errorf("BDF = %q, want %q", info.BDF, "0000:00:08.0")
	}
	if gotBody.Path != cfg.Path || gotBody.ID != cfg.ID {
		t.Errorf("server received %+v, want %+v", gotBody, cfg)
	}
}

func TestAddDevice_ColdAdd_Returns204WithNoBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.add-device", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	info, err := c.AddDevice(context.Background(), DeviceConfig{Path: "/sys/bus/pci/devices/0000:01:00.0/"})
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	if info != (PciDeviceInfo{}) {
		t.Errorf("expected zero PciDeviceInfo on a 204, got %+v", info)
	}
}

func TestAddDevice_ConflictReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.add-device", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"device with id already exists"}`))
	})
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	_, err := c.AddDevice(context.Background(), DeviceConfig{Path: "/sys/bus/pci/devices/0000:01:00.0/", ID: "dup"})
	if err == nil {
		t.Fatal("expected an error on 409 Conflict, got nil")
	}
}

// TestAddDevice_DuplicatePathRecoversRealBDF covers a real finding
// (2026-11-14): cloud-hypervisor can reject vm.add-device with "Duplicated
// device path" when the device is genuinely already attached -- including
// on a freshly-resumed VM whose restore-from-checkpoint path baked the
// device into its config from the start, confirmed live against a real
// VM. AddDevice must recover the real BDF from vm.info rather than treat
// this as a hard, permanent failure.
func TestAddDevice_DuplicatePathRecoversRealBDF(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.add-device", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Duplicated device path: /sys/bus/pci/devices/0000:01:00.0/"}`))
	})
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(VMInfo{
			Config:     VMConfig{Devices: []DeviceConfig{{ID: "gpu-0", Path: "/sys/bus/pci/devices/0000:01:00.0/"}}},
			DeviceTree: map[string]DeviceTreeNode{"gpu-0": {PciBdf: "0000:00:07.0"}},
		})
	})
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	info, err := c.AddDevice(context.Background(), DeviceConfig{Path: "/sys/bus/pci/devices/0000:01:00.0/", ID: "gpu-0"})
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	if info.ID != "gpu-0" || info.BDF != "0000:00:07.0" {
		t.Errorf("got %+v, want ID=gpu-0 BDF=0000:00:07.0", info)
	}
}

// TestAddDevice_DuplicatePathErrorsIfNotPresent is the regression guard: a
// "Duplicated device path" error must NOT be treated as success unless
// vm.info genuinely confirms the device is there. (Kept short: Windows
// unix sockets cap the path length, and t.TempDir() embeds the test name.)
func TestAddDevice_DuplicatePathErrorsIfNotPresent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.add-device", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Duplicated device path: /sys/bus/pci/devices/0000:01:00.0/"}`))
	})
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(VMInfo{})
	})
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	if _, err := c.AddDevice(context.Background(), DeviceConfig{Path: "/sys/bus/pci/devices/0000:01:00.0/", ID: "gpu-0"}); err == nil {
		t.Fatal("expected an error when vm.info does not actually list the device, got nil")
	}
}

func TestRemoveDevice_Returns204(t *testing.T) {
	mux := http.NewServeMux()
	var gotBody struct {
		ID string `json:"id"`
	}
	mux.HandleFunc("/api/v1/vm.remove-device", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, r *http.Request) {
		// The device is already gone from the VM's config by the time
		// RemoveDevice's own confirmation poll checks -- the common case.
		_ = json.NewEncoder(w).Encode(VMInfo{})
	})
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	if err := c.RemoveDevice(context.Background(), "gnwes-gpu-0"); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	if gotBody.ID != "gnwes-gpu-0" {
		t.Errorf("server received id %q, want %q", gotBody.ID, "gnwes-gpu-0")
	}
}

// TestRemoveDevice_PollsVMInfoUntilDeviceGone covers the real bug found live
// (2026-11-14): a successful vm.remove-device response does not reliably
// mean cloud-hypervisor has actually finished removing the device from its
// own bookkeeping yet. RemoveDevice must keep polling vm.info rather than
// trusting the PUT's status code alone.
func TestRemoveDevice_PollsVMInfoUntilDeviceGone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.remove-device", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	var infoCalls int
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, r *http.Request) {
		infoCalls++
		info := VMInfo{}
		if infoCalls < 3 {
			// Still present for the first two polls, simulating
			// cloud-hypervisor's own async cleanup still in flight.
			info.Config.Devices = []DeviceConfig{{ID: "gnwes-gpu-0", Path: "/sys/bus/pci/devices/0000:01:00.0/"}}
		}
		_ = json.NewEncoder(w).Encode(info)
	})
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	if err := c.RemoveDevice(context.Background(), "gnwes-gpu-0"); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	if infoCalls < 3 {
		t.Errorf("expected RemoveDevice to poll vm.info at least 3 times, got %d", infoCalls)
	}
}

// TestRemoveDevice_ErrorsIfNeverGone is the regression guard for the other
// direction: RemoveDevice must NOT report success (letting a caller
// release the device back to a pool for immediate reuse) while vm.info
// still lists it, no matter how long it waits. (Kept short: Windows unix
// sockets cap the path length, and t.TempDir() embeds the test name.)
func TestRemoveDevice_ErrorsIfNeverGone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.remove-device", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/v1/vm.info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(VMInfo{Config: VMConfig{
			Devices: []DeviceConfig{{ID: "gnwes-gpu-0", Path: "/sys/bus/pci/devices/0000:01:00.0/"}},
		}})
	})
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	if err := c.RemoveDevice(context.Background(), "gnwes-gpu-0"); err == nil {
		t.Fatal("expected an error when the device never disappears from vm.info, got nil")
	}
}

func TestRemoveDevice_NotFoundReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vm.remove-device", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	err := c.RemoveDevice(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("expected an error on 404, got nil")
	}
}
