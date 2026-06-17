// Package keymgr manages the SSH keypair used by the helper for FRS SFTP auth.
package keymgr

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	fieldPrivate = "ssh-privatekey"
	fieldPublic  = "ssh-publickey"
)

// Manager holds the loaded SSH signer and a public-key PEM suitable for
// embedding in FileRecoverySession.spec.transports.sftp.userPublicKey.
type Manager struct {
	Signer    ssh.Signer
	PublicKey ssh.PublicKey
	PubKeyPEM []byte

	// signerRaw is the raw PEM of the private key. Exposed (package-private)
	// for tests that need to re-seed the fake clientset.
	signerRaw []byte
}

// LoadOrGenerate reads Secret ns/name; if missing or partial, generates or
// derives to make it complete, then returns the Manager.
func LoadOrGenerate(ctx context.Context, cli kubernetes.Interface, ns, name string) (*Manager, error) {
	secrets := cli.CoreV1().Secrets(ns)
	sec, err := secrets.Get(ctx, name, metav1.GetOptions{})

	switch {
	case apierrors.IsNotFound(err):
		return generateAndPersist(ctx, secrets, ns, name)
	case err != nil:
		// Surface an actionable hint alongside the raw K8s client
		// error. The most common "get secret" failures we see in
		// the field are (a) an egress NetworkPolicy blocking the
		// helper pod from reaching the API server ("dial tcp
		// 172.30.0.1:443: i/o timeout") and (b) an RBAC denial
		// ("secrets is forbidden"). The raw wrapped error already
		// contains enough detail; the hint just points the operator
		// at the two manual workaround sections in DEPLOY.md.
		return nil, fmt.Errorf("get secret %s/%s: %w (hint: see DEPLOY.md §3.1 to pre-create the keypair manually, or §9 for the egress/RBAC error you are seeing)", ns, name, err)
	}

	priv := sec.Data[fieldPrivate]
	pub := sec.Data[fieldPublic]

	switch {
	case len(priv) == 0 && len(pub) == 0:
		return generateAndPersist(ctx, secrets, ns, name)
	case len(priv) == 0:
		return nil, fmt.Errorf("secret %s/%s has public key but no private key; refusing to operate", ns, name)
	case len(pub) == 0:
		return deriveAndPatch(ctx, secrets, sec, priv)
	default:
		return parseInto(priv, pub)
	}
}

func generateAndPersist(ctx context.Context, secrets corev1client.SecretInterface, ns, name string) (*Manager, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	privPEM, pubPEM, err := marshalEd25519(priv)
	if err != nil {
		return nil, err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeSSHAuth,
		Data: map[string][]byte{
			fieldPrivate: privPEM,
			fieldPublic:  pubPEM,
		},
	}
	if _, err := secrets.Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create secret: %w", err)
	}
	return parseInto(privPEM, pubPEM)
}

func deriveAndPatch(ctx context.Context, secrets corev1client.SecretInterface, sec *corev1.Secret, priv []byte) (*Manager, error) {
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	pubPEM := ssh.MarshalAuthorizedKey(signer.PublicKey())
	sec.Data[fieldPublic] = pubPEM
	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("update secret: %w", err)
	}
	return &Manager{Signer: signer, PublicKey: signer.PublicKey(), PubKeyPEM: pubPEM, signerRaw: priv}, nil
}

func parseInto(priv, pub []byte) (*Manager, error) {
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	var pubKey ssh.PublicKey
	if len(pub) > 0 {
		k, _, _, _, err := ssh.ParseAuthorizedKey(pub)
		if err != nil {
			return nil, fmt.Errorf("parse public key: %w", err)
		}
		pubKey = k
	} else {
		pubKey = signer.PublicKey()
	}
	return &Manager{Signer: signer, PublicKey: pubKey, PubKeyPEM: ssh.MarshalAuthorizedKey(pubKey), signerRaw: priv}, nil
}

func marshalEd25519(priv ed25519.PrivateKey) (privPEM, pubPEM []byte, err error) {
	pemBlock, err := ssh.MarshalPrivateKey(priv, "kasten-frs-web-helper")
	if err != nil {
		return nil, nil, err
	}
	privPEM = pem.EncodeToMemory(pemBlock)
	pubKey, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, nil, errors.New("ed25519 public key type assertion failed")
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, nil, err
	}
	pubPEM = ssh.MarshalAuthorizedKey(sshPub)
	return privPEM, pubPEM, nil
}
