// Command substrate-gpu-bridge polls Agent Substrate's actor lifecycle state
// (via `kubectl-ate get actor -o json`, since Substrate's Actor isn't a
// Kubernetes CRD you can watch directly -- only WorkerPool is) for whichever
// actor currently occupies one worker pod, and drives that pod's
// VMGPUAttachment accordingly: Attached (with a fresh api-socket path for
// that actor's VM) while it is ACTOR_STATE_RUNNING, Detached otherwise. This
// is the missing piece config/vmgpu/README.md names: VMGPUAttachment itself
// (real hot attach/detach against cloud-hypervisor) already works; this is
// what decides WHEN to flip it, so a real Substrate resume/suspend cycle can
// drive it instead of a manual `kubectl patch`.
//
// Deployed as a sidecar in the exact worker pod it watches (one bridge
// instance per Exclusive-tier worker pod), because a worker pod hosts
// actors sequentially, not just the one it started with -- there is no
// single fixed actor name to pass at deploy time, only the pod's own name,
// known at deploy time via the Kubernetes Downward API.
//
// See internal/substratebridge for the actual decision logic (fully
// unit-tested without a real kubectl-ate binary or cluster); this file is
// just the real-world wiring: exec kubectl-ate, patch VMGPUAttachment via a
// controller-runtime client, log transitions, poll forever.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
	"gnwes/internal/substratebridge"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gnwesv1alpha1.AddToScheme(scheme))
}

func main() {
	kubectlAteBin := flag.String("kubectl-ate-bin", "kubectl-ate", "path to the kubectl-ate binary")
	kubectlAteKubeconfig := flag.String("kubectl-ate-kubeconfig", "", "if set, passed to kubectl-ate as --kubeconfig (Substrate's own control-plane kubeconfig, typically different from the one pointing at the GNWES-managed cluster)")
	atespace := flag.String("atespace", "", "the atespace to watch for whichever actor is currently assigned to -worker-pod (required)")
	workerPod := flag.String("worker-pod", "", "name of the worker pod this bridge is deployed alongside (as a sidecar); also the VMGPUAttachment name, since GPUWorkerPoolReconciler creates one attachment per worker pod, named after it (required; typically $(POD_NAME) via the Downward API)")
	vcVMID := flag.String("vcvm-id", "", "short per-pod token identifying this pod's host-side api-socket subdirectory (required; see substratebridge.HostAPISocketPath -- deliberately NOT the pod's own name, which is routinely too long for a unix socket path)")
	attachmentNamespace := flag.String("attachment-namespace", "", "namespace of the VMGPUAttachment to drive (required; typically $(POD_NAMESPACE) via the Downward API)")
	pollInterval := flag.Duration("poll-interval", time.Second, "how often to poll the actor's state")
	flag.Parse()

	if *atespace == "" || *workerPod == "" || *vcVMID == "" || *attachmentNamespace == "" {
		fmt.Fprintln(os.Stderr, "all of -atespace, -worker-pod, -vcvm-id, -attachment-namespace are required")
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := config.GetConfig()
	if err != nil {
		log.Fatalf("getting kubeconfig for the cluster the VMGPUAttachment lives in: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("building Kubernetes client: %v", err)
	}

	fetcher := &substratebridge.KubectlAteFetcher{
		Binary:     *kubectlAteBin,
		Atespace:   *atespace,
		WorkerPod:  *workerPod,
		VCVMID:     *vcVMID,
		Kubeconfig: *kubectlAteKubeconfig,
	}
	driver := &k8sAttachmentDriver{
		client:    c,
		namespace: *attachmentNamespace,
		name:      *workerPod,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("substrate-gpu-bridge watching atespace %s for whichever actor is assigned to worker pod %s, driving VMGPUAttachment %s/%s, every %s",
		*atespace, *workerPod, *attachmentNamespace, *workerPod, *pollInterval)

	substratebridge.Run(ctx, fetcher, driver, *pollInterval,
		func(t substratebridge.Transition) {
			log.Printf("actor state %s -> desiredState %s apiSocket %s", t.ActorState, t.DesiredState, t.APISocket)
		},
		func(err error) {
			log.Printf("reconcile error (will retry next poll): %v", err)
		},
	)

	log.Println("substrate-gpu-bridge shutting down")
}

// k8sAttachmentDriver implements substratebridge.AttachmentDriver against a
// real VMGPUAttachment object.
type k8sAttachmentDriver struct {
	client    client.Client
	namespace string
	name      string
}

func (d *k8sAttachmentDriver) GetSpec(ctx context.Context) (gnwesv1alpha1.VMGPUDesiredState, string, error) {
	var att gnwesv1alpha1.VMGPUAttachment
	if err := d.client.Get(ctx, types.NamespacedName{Namespace: d.namespace, Name: d.name}, &att); err != nil {
		return "", "", fmt.Errorf("getting VMGPUAttachment %s/%s: %w", d.namespace, d.name, err)
	}
	return att.Spec.DesiredState, att.Spec.APISocket, nil
}

func (d *k8sAttachmentDriver) SetSpec(ctx context.Context, state gnwesv1alpha1.VMGPUDesiredState, apiSocket string) error {
	var att gnwesv1alpha1.VMGPUAttachment
	if err := d.client.Get(ctx, types.NamespacedName{Namespace: d.namespace, Name: d.name}, &att); err != nil {
		return fmt.Errorf("getting VMGPUAttachment %s/%s: %w", d.namespace, d.name, err)
	}
	patch := client.MergeFrom(att.DeepCopy())
	att.Spec.DesiredState = state
	att.Spec.APISocket = apiSocket
	if err := d.client.Patch(ctx, &att, patch); err != nil {
		return fmt.Errorf("patching VMGPUAttachment %s/%s to desiredState=%s apiSocket=%s: %w", d.namespace, d.name, state, apiSocket, err)
	}
	return nil
}
