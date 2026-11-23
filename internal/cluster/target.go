package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/k8s-ai/k8s-ai/internal/store"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// ExecutionConfig returns only the selected context, with embedded credentials.
// Uploaded kubeconfigs cannot execute a local program or reference a server file.
func (s *Service) ExecutionConfig(ctx context.Context, id string) ([]byte, string, error) {
	if s == nil {
		return nil, "", errors.New("cluster credentials unavailable")
	}
	if _, err := s.clusters.Get(ctx, id); err != nil {
		return nil, "", err
	}
	cred, err := s.credentials.GetByCluster(ctx, id)
	if err != nil {
		return nil, "", err
	}
	data, err := s.decrypt(cred)
	if err != nil {
		return nil, "", err
	}
	raw, err := clientcmd.Load(data)
	if err != nil {
		return nil, "", errors.New("invalid stored kubeconfig")
	}
	if err = api.MinifyConfig(raw); err != nil {
		return nil, "", errors.New("invalid cluster context")
	}
	if err = validatePortable(raw); err != nil {
		return nil, "", err
	}
	data, err = clientcmd.Write(*raw)
	return data, credentialVersion(cred), err
}
func validatePortable(raw *api.Config) error {
	for _, u := range raw.AuthInfos {
		if u == nil || u.Exec != nil || u.AuthProvider != nil || u.TokenFile != "" || u.ClientKey != "" || u.ClientCertificate != "" || u.Impersonate != "" || len(u.ImpersonateGroups) > 0 || len(u.ImpersonateUserExtra) > 0 {
			return errors.New("uploaded kubeconfig requires embedded credentials without exec or impersonation")
		}
	}
	for _, c := range raw.Clusters {
		if c == nil || c.CertificateAuthority != "" {
			return errors.New("uploaded kubeconfig requires embedded certificate authority")
		}
	}
	return nil
}
func credentialVersion(c *store.Credential) string {
	h := sha256.Sum256([]byte(c.KubeconfigSecret))
	return hex.EncodeToString(h[:])
}
func (s *Service) ExecutionVersion(ctx context.Context, id string) (string, error) {
	if s == nil {
		return "", errors.New("cluster unavailable")
	}
	if _, err := s.clusters.Get(ctx, id); err != nil {
		return "", err
	}
	c, err := s.credentials.GetByCluster(ctx, id)
	if err != nil {
		return "", err
	}
	return credentialVersion(c), nil
}
