package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/cluster"
)

// enterpriseIssuer 起一个由自建根 CA 签发证书的假 IdP，复刻「企业内网 IdP 用集群
// 自签 CA」这一现场：镜像的系统信任链不认它，必须靠 platform.auth.oidc_ca_file。
func enterpriseIssuer(t *testing.T) (issuer string, caPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "aegisops-enterprise-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "idp.internal"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	mux := http.NewServeMux()
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	var doc map[string]any
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		doc = map[string]any{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/auth", "token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}}
		_ = json.NewEncoder(w).Encode(doc)
	})
	return srv.URL, caPEM
}

func TestEnterpriseLoginRejectsUntrustedEnterpriseCA(t *testing.T) {
	issuer, _ := enterpriseIssuer(t)
	st := newStore(t)
	svc := New(st.Users(), st.Roles())
	login, err := NewEnterpriseLogin(context.Background(), svc, st.Settings(), nil, "https://platform.test", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ProviderConfig{Enabled: true, Issuer: issuer, ClientID: "client", Secret: "secret"}
	// 系统信任链不认自建 CA：报错必须把根因带出来，否则现场只能靠猜。
	err = login.Configure(context.Background(), cfg, true)
	if err == nil {
		t.Fatal("untrusted enterprise CA accepted")
	}
	if !strings.Contains(err.Error(), "OIDC discovery failed") || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("discovery error lost its root cause: %v", err)
	}
}

func TestEnterpriseLoginTrustsConfiguredCA(t *testing.T) {
	issuer, caPEM := enterpriseIssuer(t)
	st := newStore(t)
	svc := New(st.Users(), st.Roles())
	cipher, err := cluster.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	login, err := NewEnterpriseLogin(context.Background(), svc, st.Settings(), cipher, "https://platform.test", caPEM, false, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ProviderConfig{Enabled: true, Issuer: issuer, ClientID: "client", Secret: "secret"}
	if err := login.Configure(context.Background(), cfg, true); err != nil {
		t.Fatalf("configured CA not trusted: %v", err)
	}
}

func TestEnterpriseLoginRejectsGarbageCA(t *testing.T) {
	st := newStore(t)
	svc := New(st.Users(), st.Roles())
	if _, err := NewEnterpriseLogin(context.Background(), svc, st.Settings(), nil, "https://platform.test", []byte("not a certificate"), false, false); err == nil {
		t.Fatal("non-PEM CA content accepted")
	}
	// 空值是「不追加信任」而不是「追加一把坏信任」：必须照常起得来。
	if _, err := NewEnterpriseLogin(context.Background(), svc, st.Settings(), nil, "https://platform.test", []byte("  \n"), false, false); err != nil {
		t.Fatal(err)
	}
}
