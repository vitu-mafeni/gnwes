package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := gnwesv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding gnwes scheme: %v", err)
	}
	return scheme
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(
			&gnwesv1alpha1.GPUWorkerPool{},
			&gnwesv1alpha1.GPULease{},
			&gnwesv1alpha1.VFIOGPUPool{},
			&gnwesv1alpha1.VMGPUAttachment{},
		).
		WithObjects(objs...).
		Build()
}

func newTestPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "pause"}},
		},
	}
}
