// Command podgate-webhook is a mutating admission webhook. Agent Substrate's
// WorkerPool API has no field for scheduling gates, so pods it generates from
// a GNWES-gated WorkerPool template (carrying the gnwes.io/gpu-pool label)
// never get GNWES's gnwes.io/gpu-capacity gate added at creation time by any
// other means. This webhook is the standard Kubernetes extension point for
// exactly that: it injects the gate on Pod CREATE for any pod labeled
// gnwes.io/gpu-pool, before the apiserver persists it. PodGateReconciler
// (internal/controller) is still the only thing that ever removes the gate.
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

const (
	labelPoolRef = "gnwes.io/gpu-pool"
	gateName     = "gnwes.io/gpu-capacity"
)

var codecs = serializer.NewCodecFactory(runtime.NewScheme())

func main() {
	certFile := envOr("TLS_CERT_FILE", "/certs/tls.crt")
	keyFile := envOr("TLS_KEY_FILE", "/certs/tls.key")

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", handleMutate)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Addr: ":8443", Handler: mux}
	log.Printf("gnwes podgate-webhook listening on :8443 (cert=%s)", certFile)
	log.Fatal(srv.ListenAndServeTLS(certFile, keyFile))
}

func handleMutate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var review admissionv1.AdmissionReview
	if _, _, err := codecs.UniversalDeserializer().Decode(body, nil, &review); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp := &admissionv1.AdmissionResponse{UID: review.Request.UID, Allowed: true}

	var pod corev1.Pod
	if err := json.Unmarshal(review.Request.Object.Raw, &pod); err == nil {
		if _, needsGating := pod.Labels[labelPoolRef]; needsGating && !hasGate(&pod) {
			patch := []map[string]any{{
				"op":    "add",
				"path":  "/spec/schedulingGates",
				"value": []map[string]string{{"name": gateName}},
			}}
			patchBytes, _ := json.Marshal(patch)
			pt := admissionv1.PatchTypeJSONPatch
			resp.Patch = patchBytes
			resp.PatchType = &pt
			log.Printf("gating pod %s/%s (label %s=%s)", pod.Namespace, pod.Name, labelPoolRef, pod.Labels[labelPoolRef])
		}
	}

	review.Response = resp
	out, _ := json.Marshal(review)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func hasGate(pod *corev1.Pod) bool {
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name == gateName {
			return true
		}
	}
	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
