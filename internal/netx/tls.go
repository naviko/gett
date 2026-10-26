package netx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// TLS prints the certificate host presents: subject, names, issuer,
// validity, protocol and cipher. Port 443 unless host has one.
func TLS(ctx context.Context, target string) error {
	if strings.Contains(target, "://") {
		target = hostOf(target)
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		host, port = target, "443"
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: connectTimeout}, Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}
	trust := ""
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		if !isCertError(err) {
			return err
		}
		trust = "not trusted: " + strings.TrimPrefix(err.Error(), "tls: failed to verify certificate: ")
		d.Config = &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // only to show the certificate
		if c, err = d.DialContext(ctx, "tcp", net.JoinHostPort(host, port)); err != nil {
			return err
		}
	}
	defer c.Close()
	st := c.(*tls.Conn).ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return fmt.Errorf("%s sent no certificate", host)
	}
	cert := st.PeerCertificates[0]
	left := time.Until(cert.NotAfter)
	days := int(left.Hours() / 24)
	expiry := fmt.Sprintf("%d days left", days)
	if left < 0 {
		expiry = fmt.Sprintf("expired %d days ago", -days)
	}
	names := cert.DNSNames
	if len(names) > 5 {
		names = append(names[:5:5], fmt.Sprintf("+%d more", len(cert.DNSNames)-5))
	}
	line("%s", padRow("subject", cert.Subject.CommonName))
	if len(names) > 0 {
		line("%s", padRow("names", strings.Join(names, " ")))
	}
	line("%s", padRow("issuer", cert.Issuer.CommonName))
	line("%s", padRow("valid", cert.NotBefore.UTC().Format("2006-01-02")+" to "+cert.NotAfter.UTC().Format("2006-01-02")+", "+expiry))
	line("%s", padRow("protocol", tls.VersionName(st.Version)))
	line("%s", padRow("cipher", tls.CipherSuiteName(st.CipherSuite)))
	if trust != "" {
		line("%s", padRow("trust", trust))
	}
	return nil
}

func isCertError(err error) bool {
	var ce *tls.CertificateVerificationError
	return errors.As(err, &ce) || strings.Contains(err.Error(), "certificate")
}
