package ftpx

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
)

// selfSigned makes a certificate for 127.0.0.1 and points the client at it.
func selfSigned(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gett test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	old := tlsConfig
	tlsConfig = func(host string) *tls.Config {
		return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	t.Cleanup(func() { tlsConfig = old })
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

type tlsDriver struct {
	*driver
	cfg      *tls.Config
	implicit bool
}

func (d *tlsDriver) GetTLSConfig() (*tls.Config, error) { return d.cfg, nil }
func (d *tlsDriver) GetSettings() (*ftpserver.Settings, error) {
	s, _ := d.driver.GetSettings()
	if d.implicit {
		s.TLSRequired = ftpserver.ImplicitEncryption
	} else {
		s.TLSRequired = ftpserver.MandatoryEncryption // plain logins are refused: TLS must really be used
	}
	return s, nil
}

func startTLSServer(t *testing.T, implicit bool) (*driver, string) {
	t.Helper()
	d := &driver{fs: afero.NewMemMapFs()}
	srv := ftpserver.NewFtpServer(&tlsDriver{d, selfSigned(t), implicit})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Stop() })
	return d, srv.Addr()
}

func TestTLS(t *testing.T) {
	onTerminal(t)
	for _, implicit := range []bool{false, true} {
		d, addr := startTLSServer(t, implicit)
		scheme := "ftp://"
		if implicit {
			scheme = "ftps://"
		}
		dir, files := makeTree(t)
		url := scheme + "u:p@" + addr + "/up"
		if err := Upload(context.Background(), url, dir); err != nil {
			t.Fatalf("implicit=%v upload: %v", implicit, err)
		}
		for name, want := range files {
			if got, _ := afero.ReadFile(d.fs, "/up/"+name); !bytes.Equal(got, want) {
				t.Fatalf("implicit=%v: %s differs on server", implicit, name)
			}
		}
		out := t.TempDir()
		if _, err := Download(context.Background(), url+"/sub/b.bin", out+"/"); err != nil {
			t.Fatalf("implicit=%v download: %v", implicit, err)
		}
		if !bytes.Equal(readLocal(t, filepath.Join(out, "b.bin")), files["sub/b.bin"]) {
			t.Fatalf("implicit=%v: downloaded content differs", implicit)
		}
		var buf bytes.Buffer
		if err := List(context.Background(), url+"/", &buf); err != nil || !bytes.Contains(buf.Bytes(), []byte("sub/")) {
			t.Fatalf("implicit=%v list: %q %v", implicit, buf.String(), err)
		}
	}
}
