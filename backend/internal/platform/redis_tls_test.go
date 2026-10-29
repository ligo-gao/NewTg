package platform

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件锁定 REDIS_TLS_CA 的语义：只有 rediss:// 才接受该变量；PEM 必须
// 含 CA 证书；文件缺失或无法解析一律返回错误（fail-closed）。跨机部署时
// Redis 用私有 CA 签发的证书，缺少这条注入路径就只能靠改基础镜像的证书库。

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func encodeCert(t *testing.T, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// newTestCA 返回自签 CA 的 PEM、解析后的证书与其私钥。
func newTestCA(t *testing.T) (pemBytes []byte, cert *x509.Certificate, key *ecdsa.PrivateKey) {
	t.Helper()
	key = testKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "newtg-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return encodeCert(t, der), cert, key
}

// newTestLeaf 返回一枚由 ca 签发的服务端叶子证书 PEM，以及非 CA 自签证书 PEM。
func newTestLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) (leafPEM []byte, leafCert *x509.Certificate) {
	t.Helper()
	key := testKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "redis"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	leafCert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return encodeCert(t, der), leafCert
}

func writeTempPEM(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write temp pem: %v", err)
	}
	return path
}

func TestConfigureRedisTLS(t *testing.T) {
	caPEM, _, _ := newTestCA(t)
	caPath := writeTempPEM(t, caPEM)
	garbagePath := writeTempPEM(t, []byte("not a pem"))

	cases := []struct {
		name    string
		opt     *redis.Options
		caPath  string
		wantErr bool
	}{
		{name: "空路径不改动配置", opt: &redis.Options{}, caPath: ""},
		{name: "非 rediss 却设了 CA", opt: &redis.Options{}, caPath: caPath, wantErr: true},
		{name: "文件不存在", opt: &redis.Options{TLSConfig: &tls.Config{}}, caPath: filepath.Join(t.TempDir(), "missing.pem"), wantErr: true},
		{name: "PEM 无法解析", opt: &redis.Options{TLSConfig: &tls.Config{}}, caPath: garbagePath, wantErr: true},
		{name: "合法 CA", opt: &redis.Options{TLSConfig: &tls.Config{}}, caPath: caPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := configureRedisTLS(tc.opt, tc.caPath)
			if tc.wantErr && err == nil {
				t.Fatalf("configureRedisTLS() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("configureRedisTLS() error = %v", err)
			}
		})
	}
}

// 合法 CA 必须真正进入 RootCAs：由该 CA 签发的叶子证书要能通过校验。
func TestConfigureRedisTLSTrustsPrivateCA(t *testing.T) {
	caPEM, caCert, caKey := newTestCA(t)
	caPath := writeTempPEM(t, caPEM)
	_, leaf := newTestLeaf(t, caCert, caKey)

	opt := &redis.Options{TLSConfig: &tls.Config{}}
	if err := configureRedisTLS(opt, caPath); err != nil {
		t.Fatalf("configureRedisTLS() error = %v", err)
	}
	if opt.TLSConfig.RootCAs == nil {
		t.Fatal("RootCAs is nil after configureRedisTLS")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: opt.TLSConfig.RootCAs}); err != nil {
		t.Fatalf("leaf signed by configured CA failed verification: %v", err)
	}
}

// PEM 里只有叶子证书（IsCA=false）时必须报错，不能带着"没有 CA 的根池"继续跑。
func TestConfigureRedisTLSRejectsNonCACert(t *testing.T) {
	key := testKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "not-a-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create self-signed leaf: %v", err)
	}
	path := writeTempPEM(t, encodeCert(t, der))

	if err := configureRedisTLS(&redis.Options{TLSConfig: &tls.Config{}}, path); err == nil {
		t.Fatal("configureRedisTLS() = nil for non-CA PEM, want error")
	}
}
