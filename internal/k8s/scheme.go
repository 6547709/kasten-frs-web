package k8s

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// FRSGroupVersion is the group/version of the FileRecoverySession CR.
var FRSGroupVersion = schema.GroupVersion{Group: "datamover.kio.kasten.io", Version: "v1alpha1"}

// NewScheme returns a runtime.Scheme with the FileRecoverySession List kind
// registered so the dynamic fake client can enumerate FRS objects. The fake
// dynamic client needs the List GVK registered to know the kind name to use
// when listing a GVR; individual FRS objects remain unstructured.
func NewScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: FRSGroupVersion.Group, Version: FRSGroupVersion.Version, Kind: "FileRecoverySessionList"},
		&unstructured.UnstructuredList{},
	)
	return s
}