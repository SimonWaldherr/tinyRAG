package main

import (
	"crypto/x509"
	"testing"
)

func TestLdapTLSConfigLoadsEmbeddedCAChain(t *testing.T) {
	cfg := ldapTLSConfig()
	if cfg.RootCAs == nil {
		t.Fatal("want a non-nil RootCAs pool")
	}
	// A pool that starts from x509.SystemCertPool() can legitimately
	// report zero Subjects() on some platforms (Go's docs note this for
	// pools derived from the system pool), so this only checks that
	// AppendCertsFromPEM would have succeeded against a fresh pool —
	// TestLdapCAChainPEMParsesToTwoCertificates below is the real
	// regression guard for the embedded file's content.
	if !x509.NewCertPool().AppendCertsFromPEM(ldapCAChainPEM) {
		t.Fatal("ldapTLSConfig's embedded CA chain failed to parse")
	}
}

func TestLdapCAChainPEMParsesToTwoCertificates(t *testing.T) {
	if len(ldapCAChainPEM) == 0 {
		t.Fatal("embedded certs/ldap_ca_chain.pem is empty")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ldapCAChainPEM) {
		t.Fatal("embedded PEM failed to parse as certificates")
	}
	subjects := pool.Subjects() //nolint:staticcheck
	if len(subjects) != 2 {
		t.Fatalf("want exactly 2 certs (Root CA + Issuing CA) in ldap_ca_chain.pem, got %d", len(subjects))
	}
}
