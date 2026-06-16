package keymgr

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newSecret(priv, pub string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "k", Namespace: "n"},
		Type:       corev1.SecretTypeSSHAuth,
	}
	s.Data = map[string][]byte{}
	if priv != "" {
		s.Data["ssh-privatekey"] = []byte(priv)
	}
	if pub != "" {
		s.Data["ssh-publickey"] = []byte(pub)
	}
	return s
}

func TestLoadOrGenerate_NoSecret_GeneratesAndPersists(t *testing.T) {
	cli := fake.NewSimpleClientset()
	m, err := LoadOrGenerate(context.Background(), cli, "n", "k")
	if err != nil {
		t.Fatal(err)
	}
	if m.Signer == nil {
		t.Fatal("signer nil")
	}
	if len(m.PubKeyPEM) == 0 {
		t.Fatal("pubkey pem empty")
	}
	got, err := cli.CoreV1().Secrets("n").Get(context.Background(), "k", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Data["ssh-privatekey"]) == 0 {
		t.Error("private key not persisted")
	}
	if len(got.Data["ssh-publickey"]) == 0 {
		t.Error("public key not persisted")
	}
	if got.Type != corev1.SecretTypeSSHAuth {
		t.Errorf("type = %s", got.Type)
	}
}

func TestLoadOrGenerate_BothPresent_UseExisting(t *testing.T) {
	// First generate a valid pair to seed
	seedMgr, err := LoadOrGenerate(context.Background(), fake.NewSimpleClientset(), "tmp", "tmp")
	if err != nil {
		t.Fatal(err)
	}
	seedPriv := string(seedMgr.signerRaw)
	seedPub := string(seedMgr.PubKeyPEM)

	cli := fake.NewSimpleClientset(newSecret(seedPriv, seedPub))
	m, err := LoadOrGenerate(context.Background(), cli, "n", "k")
	if err != nil {
		t.Fatal(err)
	}
	if m.Signer == nil {
		t.Fatal("signer nil")
	}
	if string(m.PubKeyPEM) != seedPub {
		t.Errorf("pubkey changed: want %q got %q", seedPub, string(m.PubKeyPEM))
	}
}

func TestLoadOrGenerate_OnlyPrivate_DerivesPublic(t *testing.T) {
	seedMgr, err := LoadOrGenerate(context.Background(), fake.NewSimpleClientset(), "tmp", "tmp")
	if err != nil {
		t.Fatal(err)
	}

	cli := fake.NewSimpleClientset(newSecret(string(seedMgr.signerRaw), ""))
	m, err := LoadOrGenerate(context.Background(), cli, "n", "k")
	if err != nil {
		t.Fatal(err)
	}
	if m.Signer == nil {
		t.Fatal("signer nil")
	}
	if len(m.PubKeyPEM) == 0 {
		t.Fatal("pubkey not derived")
	}
	got, _ := cli.CoreV1().Secrets("n").Get(context.Background(), "k", metav1.GetOptions{})
	if len(got.Data["ssh-publickey"]) == 0 {
		t.Error("public key not written back")
	}
}

func TestLoadOrGenerate_OnlyPublic_Fails(t *testing.T) {
	cli := fake.NewSimpleClientset(newSecret("", "ssh-ed25519 AAAA... user@host"))
	_, err := LoadOrGenerate(context.Background(), cli, "n", "k")
	if err == nil {
		t.Error("expected error when only public key present")
	}
}
