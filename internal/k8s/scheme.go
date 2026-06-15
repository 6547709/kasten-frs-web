package k8s

import (
	"k8s.io/apimachinery/pkg/runtime"
)

// NewScheme returns a runtime.Scheme. Currently empty; FRS types will be
// added in Task 7. The fake dynamic client works with unstructured.Unstructured
// directly, so an empty scheme is sufficient for the FRS listing path.
func NewScheme() *runtime.Scheme {
	return runtime.NewScheme()
}