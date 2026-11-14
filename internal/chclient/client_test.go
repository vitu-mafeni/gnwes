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
	sock := newTestServer(t, mux)

	c := NewClient(sock)
	if err := c.RemoveDevice(context.Background(), "gnwes-gpu-0"); err != nil {
		t.Fatalf("RemoveDevice: %v", err)
	}
	if gotBody.ID != "gnwes-gpu-0" {
		t.Errorf("server received id %q, want %q", gotBody.ID, "gnwes-gpu-0")
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
