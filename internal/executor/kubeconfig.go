package executor

import "os"

// TemporaryKubeconfig provides the same explicit target to kubectl and client-go.
func TemporaryKubeconfig(data []byte) (string, func(), error) {
	f, err := os.CreateTemp("", "aegisops-cluster-*.yaml")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err = f.Write(data); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err = f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}
