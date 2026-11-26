package web

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The panel's own port speaks HTTPS from the first start, with a self-signed
// certificate, so the setup token and the first login never cross the
// network in the clear. The same port still answers plain HTTP for local
// clients: Skiff's Nginx proxy (once the panel has a real domain) and SSH
// tunnels. Plain HTTP from anywhere else is redirected to HTTPS (ServeHTTP).

const (
	tlsCertFile = "panel-tls.crt"
	tlsKeyFile  = "panel-tls.key"
)

// PanelTLS loads the panel's self-signed certificate from dataDir, creating it
// on first use, and returns a TLS config and its SHA-256 fingerprint.
func PanelTLS(dataDir string) (*tls.Config, string, error) {
	certPath, keyPath := filepath.Join(dataDir, tlsCertFile), filepath.Join(dataDir, tlsKeyFile)
	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		if err := writeSelfSigned(certPath, keyPath); err != nil {
			return nil, "", fmt.Errorf("create the panel's TLS certificate: %w", err)
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, "", err
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	return cfg, Fingerprint(cert.Certificate[0]), nil
}

// TLSFingerprint returns the fingerprint of the certificate in dataDir.
func TLSFingerprint(dataDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, tlsCertFile))
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return "", errors.New("no certificate in " + tlsCertFile)
	}
	return Fingerprint(block.Bytes), nil
}

// Fingerprint formats a certificate's SHA-256 the way browsers show it.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

func writeSelfSigned(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Skiff panel", Organization: []string{"Skiff (self-signed)"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	if host != "" && host != "localhost" {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ipn.IP)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// MuxListener serves TLS and plain HTTP on one port: a connection whose first
// byte starts a TLS handshake (0x16) gets TLS, anything else is passed on as
// is. Each connection is sniffed in its own goroutine, so a slow client can't
// hold up the others.
type MuxListener struct {
	net.Listener
	tls   *tls.Config
	conns chan net.Conn
	errs  chan error
	done  chan struct{}
	once  sync.Once
}

func NewMuxListener(l net.Listener, cfg *tls.Config) *MuxListener {
	m := &MuxListener{Listener: l, tls: cfg, conns: make(chan net.Conn), errs: make(chan error), done: make(chan struct{})}
	go m.loop()
	return m
}

func (m *MuxListener) loop() {
	for {
		c, err := m.Listener.Accept()
		if err != nil {
			select {
			case m.errs <- err:
			case <-m.done:
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go m.sniff(c)
	}
}

func (m *MuxListener) sniff(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	first, err := br.Peek(1)
	if err != nil {
		c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	var out net.Conn = &peekedConn{Conn: c, r: br}
	if first[0] == 0x16 {
		out = tls.Server(out, m.tls)
	}
	select {
	case m.conns <- out:
	case <-m.done:
		c.Close()
	}
}

func (m *MuxListener) Accept() (net.Conn, error) {
	select {
	case c := <-m.conns:
		return c, nil
	case err := <-m.errs:
		return nil, err
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *MuxListener) Close() error {
	m.once.Do(func() { close(m.done) })
	return m.Listener.Close()
}

// peekedConn replays the sniffed byte before reading from the connection.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }
