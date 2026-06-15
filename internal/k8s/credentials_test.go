package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestLoadPrivateKey_Success(t *testing.T) {
	core := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kasten-frs-helper-private-key", Namespace: "kasten-io"},
		Type:       corev1.SecretTypeSSHAuth,
		Data:       map[string][]byte{"ssh-privatekey": []byte(samplePEM)},
	})
	c := &Client{core: core}

	cfg := CredentialsConfig{
		Namespace: "kasten-io",
		Name:      "kasten-frs-helper-private-key",
		Field:     "ssh-privatekey",
	}
	creds, err := c.LoadPrivateKey(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Username != "root" {
		t.Errorf("username = %q, want root", creds.Username)
	}
	if len(creds.PrivateKey) == 0 {
		t.Error("private key empty")
	}
}

func TestLoadPrivateKey_WrongType(t *testing.T) {
	core := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kasten-frs-helper-private-key", Namespace: "kasten-io"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"ssh-privatekey": []byte("ignored")},
	})
	c := &Client{core: core}
	_, err := c.LoadPrivateKey(context.Background(), CredentialsConfig{
		Namespace: "kasten-io", Name: "kasten-frs-helper-private-key", Field: "ssh-privatekey",
	})
	if err == nil {
		t.Fatal("expected error for wrong secret type")
	}
}

func TestLoadPrivateKey_MissingField(t *testing.T) {
	core := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kasten-frs-helper-private-key", Namespace: "kasten-io"},
		Type:       corev1.SecretTypeSSHAuth,
		Data:       map[string][]byte{},
	})
	c := &Client{core: core}
	_, err := c.LoadPrivateKey(context.Background(), CredentialsConfig{
		Namespace: "kasten-io", Name: "kasten-frs-helper-private-key", Field: "ssh-privatekey",
	})
	if err == nil {
		t.Fatal("expected error for missing field")
	}
}

const samplePEM = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACDExampleKeyMaterialNotARealKeyForTestingPurposesOnlyAAAA
-----END OPENSSH PRIVATE KEY-----
`
