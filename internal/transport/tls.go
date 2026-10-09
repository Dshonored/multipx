// Package transport holds the QUIC/TLS plumbing shared by the daemon and
// the client.
package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"time"

	"github.com/quic-go/quic-go"
)

// ALPN is the application protocol both sides negotiate over QUIC/TLS.
const ALPN = "multipx/1"

// QUICConfig returns the quic.Config shared by the daemon's listener and
// every client dial.
//
// quic-go's default InitialPacketSize is 1280 bytes — which happens to
// exactly match a Tailscale tailscale0 interface's own MTU. Once IP/UDP
// headers are added on top, that packet no longer fits the tunnel, so it
// gets silently fragmented or dropped: the handshake times out with zero
// response even though the path is otherwise healthy (ICMP pings, which
// are far smaller, go through fine). A size at or under the RFC 9000
// minimum (1200) comfortably clears any real-world tunnel MTU, and
// disabling path MTU discovery stops the connection from later probing
// back up to a size that gets blackholed mid-session.
func QUICConfig() *quic.Config {
	return &quic.Config{
		InitialPacketSize:       1200,
		DisablePathMTUDiscovery: true,
	}
}

// ServerTLSConfig generates an in-memory self-signed certificate and
// returns a tls.Config ready for quic.ListenAddr.
//
// TODO: once sessions span real networks (not just loopback), replace this
// with a persisted daemon keypair and trust-on-first-use pinning on the
// client side instead of InsecureSkipVerify.
func ServerTLSConfig() (*tls.Config, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "multipx-daemon"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"multipx-daemon"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{ALPN},
	}, nil
}

// ClientTLSConfig trusts whatever certificate the daemon presents. Fine for
// a daemon on loopback; see the TODO on ServerTLSConfig for remote hosts.
func ClientTLSConfig() *tls.Config {
	return &tls.Config{
		NextProtos:         []string{ALPN},
		InsecureSkipVerify: true, //nolint:gosec // v1: loopback-only, see ServerTLSConfig TODO
	}
}
