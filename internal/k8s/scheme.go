package k8s

import (
	"k8s.io/apimachinery/pkg/runtime"
)

// NewScheme returns a runtime.Scheme that knows about the FileRecoverySession CRD.
// Concrete types (FileRecoverySession, FileRecoverySessionList) will be added
// in Task 7. For now, an empty scheme is sufficient because the fake dynamic
// client works with unstructured.Unstructured directly.
func NewScheme() *runtime.Scheme {
	return runtime.NewScheme()
}