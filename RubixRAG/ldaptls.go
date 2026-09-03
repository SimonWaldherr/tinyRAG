package main

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"log"
)

// ─────────────────────────────────────────────────────────────────────────────
// TLS trust for LDAPS.
//
// certs/ldap_ca_chain.pem is Rubix's internal PKI (Root CA + Issuing CA —
// see external/ldaps/root_ca.cer / issuing_ca.cer, copied here unchanged
// so go:embed can reach them from this module), the CA that signs
// inf-pla-04.zitec-intern.de's certificate — R3's default LDAP host (see
// settings.go's defaultSettings). Embedding it makes LDAPS binds work
// regardless of whether the host OS already trusts that CA, which it
// typically does on a domain-joined Windows machine (via Group Policy)
// but would NOT on, say, a non-domain-joined Linux container.
//
// ldapTLSConfig is additive, not a replacement: it starts from the host
// OS's own trust store and adds Rubix's CA on top, so a deployment
// pointed at a different AD environment with a publicly-trusted or
// otherwise-signed LDAPS certificate keeps working exactly as before —
// this never removes trust the OS already grants, only adds to it.
// ─────────────────────────────────────────────────────────────────────────────

//go:embed certs/ldap_ca_chain.pem
var ldapCAChainPEM []byte

// ldapTLSConfig returns a *tls.Config used for every LDAP/LDAPS dial (see
// ldapauth.go's ldapAuthenticate) — trusts the host OS's default roots
// plus the embedded Rubix CA chain above.
func ldapTLSConfig() *tls.Config {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// No usable OS trust store on this platform/runtime — fall back to
		// an empty pool rather than failing outright, so LDAPS against
		// Rubix's own CA (the common case) still works even here.
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(ldapCAChainPEM) {
		log.Printf("WARN: failed to parse embedded LDAP CA chain (certs/ldap_ca_chain.pem)")
	}
	return &tls.Config{RootCAs: pool}
}
