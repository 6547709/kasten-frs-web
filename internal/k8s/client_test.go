package k8s

import (
	"testing"
)

func TestNewClient_FakeMode(t *testing.T) {
	c, err := NewClient(ClientOptions{InCluster: false, Fake: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Core() == nil {
		t.Fatal("Core client nil")
	}
	if c.Dynamic() == nil {
		t.Fatal("Dynamic client nil")
	}
	if !c.IsFake() {
		t.Fatal("IsFake() = false, want true")
	}
}