/**
 * @Author: LiuShuXin
 * @Description: 验证 Redis TLS 证书链和主机名检查不允许明文回退
 * @File: tls_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package redis

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestTLSHandshakeVerification(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Redis test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})
	if err != nil {
		t.Fatal(err)
	}
	o := replyFixtureOn(t, listener, func(net.Conn, []string) {})
	o.TLS = true
	o.TLSRootCert = path
	c := fixtureClient(t, o, zap.NewNop())
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	bad := o
	bad.TLSRootCert = ""
	client, err := Open(context.Background(), bad, zap.NewNop())
	var authority x509.UnknownAuthorityError
	if err == nil || !errors.As(err, &authority) {
		if client != nil {
			client.Close()
		}
		t.Fatalf("untrusted certificate not rejected by verification: %v", err)
	}
	// 同一 IP 上可解析的 localhost 与证书 IP SAN 不匹配，必须拒绝。
	bad = o
	bad.Host = "localhost"
	client, err = Open(context.Background(), bad, zap.NewNop())
	var hostname x509.HostnameError
	if err == nil || !errors.As(err, &hostname) {
		if client != nil {
			client.Close()
		}
		t.Fatalf("hostname mismatch not rejected by verification: %v", err)
	}
}
