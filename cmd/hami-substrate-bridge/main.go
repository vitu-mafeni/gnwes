// Command hami-substrate-bridge polls one Agent Substrate actor's lifecycle
// state (via `kubectl-ate get actor -o json`, the same mechanism
// cmd/substrate-gpu-bridge already uses for the Exclusive/VFIO tier) and
// drives the Shared (HAMi) tier's checkpoint/restore cycle to match: while
// the actor is ACTOR_STATE_RUNNING, its HAMi-shared GPU pod exists (created
// fresh the first time, restored from the last checkpoint every time
// after); otherwise, the pod is checkpointed via FluidCR (already deployed
// on this project's real cluster) and removed, freeing its GPU slice for
// other agents.
//
// See internal/hamilifecycle for the actual decision logic and FluidCR
// wiring (unit-tested against a fake client); this file is the real-world
// wiring: exec kubectl-ate, drive FluidCRCheckpoint/FluidCRRestore via a
// controller-runtime client, log transitions, poll forever.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"

	"gnwes/internal/fluidcrclient"
	"gnwes/internal/hamilifecycle"
	"gnwes/internal/substratebridge"

	"log"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(fluidcrclient.AddToScheme(scheme))
}

func main() {
	kubectlAteBin := flag.String("kubectl-ate-bin", "kubectl-ate", "path to the kubectl-ate binary")
	kubectlAteKubeconfig := flag.String("kubectl-ate-kubeconfig", "", "if set, passed to kubectl-ate as --kubeconfig (Substrate's own control-plane kubeconfig)")
	atespace := flag.String("atespace", "", "the actor's atespace (required)")
	actorName := flag.String("actor", "", "the actor name to watch (required)")
	namespace := flag.String("namespace", "", "namespace of the HAMi-shared pod and FluidCR objects (required)")
	podName := flag.String("pod-name", "", "name of the HAMi-shared pod to manage (required)")
	containerName := flag.String("container-name", "", "the GPU-using container within the pod (required)")
	podSpecFile := flag.String("pod-spec-file", "", "path to a YAML file holding the corev1.PodSpec used both to create the pod fresh and as the FluidCRRestore template (required)")
	pollInterval := flag.Duration("poll-interval", time.Second, "how often to poll the actor's state")
	fluidcrPollInterval := flag.Duration("fluidcr-poll-interval", 2*time.Second, "how often to poll a FluidCRCheckpoint/FluidCRRestore's phase while waiting for it to finish")
	flag.Parse()

	if *atespace == "" || *actorName == "" || *namespace == "" || *podName == "" || *containerName == "" || *podSpecFile == "" {
		fmt.Fprintln(os.Stderr, "all of -atespace, -actor, -namespace, -pod-name, -container-name, -pod-spec-file are required")
		flag.Usage()
		os.Exit(2)
	}

	podSpecBytes, err := os.ReadFile(*podSpecFile)
	if err != nil {
		log.Fatalf("reading -pod-spec-file %q: %v", *podSpecFile, err)
	}
	var podSpec corev1.PodSpec
	if err := yaml.Unmarshal(podSpecBytes, &podSpec); err != nil {
		log.Fatalf("parsing -pod-spec-file %q as a corev1.PodSpec: %v", *podSpecFile, err)
	}

	cfg, err := config.GetConfig()
	if err != nil {
		log.Fatalf("getting kubeconfig for the cluster FluidCR and the pod live on: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("building Kubernetes client: %v", err)
	}

	fetcher := &substratebridge.KubectlAteFetcher{
		Binary:     *kubectlAteBin,
		Atespace:   *atespace,
		ActorName:  *actorName,
		Kubeconfig: *kubectlAteKubeconfig,
	}
	driver := &hamilifecycle.Driver{
		Client:        c,
		Namespace:     *namespace,
		PodName:       *podName,
		ContainerName: *containerName,
		PodSpec:       podSpec,
		PollInterval:  *fluidcrPollInterval,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("hami-substrate-bridge watching actor %s/%s, driving pod %s/%s, every %s",
		*atespace, *actorName, *namespace, *podName, *pollInterval)

	hamilifecycle.Run(ctx, fetcher, driver, *pollInterval,
		func(t hamilifecycle.Transition) {
			log.Printf("actor state %s -> pod running=%v", t.ActorState, t.NowRunning)
		},
		func(err error) {
			log.Printf("reconcile error (will retry next poll): %v", err)
		},
	)

	log.Println("hami-substrate-bridge shutting down")
}
