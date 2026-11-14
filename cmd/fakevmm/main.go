// Command fakevmm is a minimal stand-in for cloud-hypervisor's REST VMM API
// (vmm.ping, vm.add-device, vm.remove-device), serving the exact wire format
// internal/chclient speaks, over a real unix socket. It exists so the
// VMGPUAttachment mechanism (api/v1alpha1, internal/controller,
// internal/chclient) can be exercised end-to-end -- a real controller-runtime
// manager reconciling real Kubernetes objects and making real HTTP-over-
// unix-socket calls -- on machines without KVM/a real cloud-hypervisor
// instance available (see research/live-attach-poc/README.md: this dev
// environment has no /dev/kvm). It does NOT touch any real device; AddDevice
// just remembers the path/id it was given and hands back a made-up BDF.
//
// This is a development/testing tool, not part of the production path: a
// real deployment points VMGPUAttachment.spec.apiSocket at a real
// cloud-hypervisor instance's actual api-socket instead.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
)

type device struct {
	Path string `json:"path"`
	ID   string `json:"id"`
	BDF  string `json:"bdf"`
}

type server struct {
	mu      sync.Mutex
	devices map[string]device
	nextBDF int
}

func (s *server) ping(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":       "0.0.0-fakevmm",
		"build_version": "fakevmm",
		"features":      []string{},
	})
}

func (s *server) addDevice(w http.ResponseWriter, r *http.Request) {
	var cfg struct {
		Path  string `json:"path"`
		Iommu bool   `json:"iommu"`
		ID    string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.devices[cfg.ID]; exists {
		http.Error(w, "device with this id already exists", http.StatusConflict)
		return
	}
	s.nextBDF++
	bdf := fakeBDF(s.nextBDF)
	d := device{Path: cfg.Path, ID: cfg.ID, BDF: bdf}
	s.devices[cfg.ID] = d
	log.Printf("vm.add-device: id=%q path=%q -> guest bdf=%s", cfg.ID, cfg.Path, bdf)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": d.ID, "bdf": d.BDF})
}

// info serves GET /api/v1/vm.info, specifically its config.devices list --
// chclient.Client.RemoveDevice now polls this after a successful
// vm.remove-device to confirm a device is really gone (see RemoveDevice's
// own doc comment for the real cloud-hypervisor behavior that made this
// necessary). fakevmm's in-memory device map is always immediately
// consistent, so this never actually makes RemoveDevice poll more than
// once here -- it exists so fakevmm keeps serving every endpoint
// chclient's real transport now calls, not to reproduce the real async
// cleanup delay.
func (s *server) info(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	devices := make([]map[string]string, 0, len(s.devices))
	for _, d := range s.devices {
		devices = append(devices, map[string]string{"id": d.ID, "path": d.Path})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"config": map[string]any{"devices": devices},
	})
}

func (s *server) removeDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.devices[req.ID]; !exists {
		http.Error(w, "no device with this id", http.StatusNotFound)
		return
	}
	delete(s.devices, req.ID)
	log.Printf("vm.remove-device: id=%q", req.ID)
	w.WriteHeader(http.StatusNoContent)
}

func fakeBDF(n int) string {
	return fmt.Sprintf("0000:00:%02x.0", n)
}

func main() {
	socketPath := flag.String("socket", "", "unix socket path to serve the fake VMM API on (required)")
	flag.Parse()

	if *socketPath == "" {
		log.Fatal("-socket is required")
	}
	_ = os.Remove(*socketPath) // stale socket from a previous run

	l, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalf("listening on %s: %v", *socketPath, err)
	}
	defer l.Close()

	s := &server{devices: map[string]device{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vmm.ping", s.ping)
	mux.HandleFunc("/api/v1/vm.add-device", s.addDevice)
	mux.HandleFunc("/api/v1/vm.remove-device", s.removeDevice)
	mux.HandleFunc("/api/v1/vm.info", s.info)

	log.Printf("fakevmm serving on unix socket %s", *socketPath)
	log.Fatal(http.Serve(l, mux))
}
