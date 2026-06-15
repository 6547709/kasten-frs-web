package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CredentialsConfig describes which Secret to read.
type CredentialsConfig struct {
	Namespace string
	Name      string
	Field     string
}

// PrivateKeyCredentials is the loaded SSH private key.
type PrivateKeyCredentials struct {
	Username   string
	PrivateKey []byte
}

// LoadPrivateKey reads the SSH private key Secret. Username is fixed to "root"
// (v0.6 spec) and not derived from the Secret.
func (c *Client) LoadPrivateKey(ctx context.Context, cfg CredentialsConfig) (PrivateKeyCredentials, error) {
	secret, err := c.core.CoreV1().Secrets(cfg.Namespace).Get(ctx, cfg.Name, metav1.GetOptions{})
	if err != nil {
		return PrivateKeyCredentials{}, fmt.Errorf("get private key secret %s/%s: %w",
			cfg.Namespace, cfg.Name, err)
	}
	if secret.Type != corev1.SecretTypeSSHAuth {
		return PrivateKeyCredentials{}, fmt.Errorf(
			"secret type must be %s, got %s", corev1.SecretTypeSSHAuth, secret.Type)
	}
	pk := secret.Data[cfg.Field]
	if len(pk) == 0 {
		return PrivateKeyCredentials{}, fmt.Errorf("secret %s/%s missing field %q",
			cfg.Namespace, cfg.Name, cfg.Field)
	}
	return PrivateKeyCredentials{Username: "root", PrivateKey: pk}, nil
}
