// Package secure handles mutual TLS. Each side has its own self-signed
// certificate and trusts the other by fingerprint, so there's no shared secret,
// and a private key never leaves the machine that created it.
package secure

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	serverCert = "tls.crt"
	serverKey  = "tls.key"
	clientCert = "client.crt"
	clientKey  = "client.key"
	allowFile  = "clients.allow"
)

var fingerprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Init creates the server's certificate and key in dir. It refuses to overwrite
// them, because that would break every client's saved fingerprint.
func Init(dir string, ip net.IP) (fingerprint string, err error) {
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "tramline"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{ip},
	}
	return generate(dir, serverCert, serverKey, tmpl)
}

// KeyGen creates a client's certificate and key in dir. Only the fingerprint is
// returned, which is public and is what gets allowed on the server.
func KeyGen(dir string) (fingerprint string, err error) {
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "tramline-client"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	return generate(dir, clientCert, clientKey, tmpl)
}

func generate(dir, certName, keyName string, tmpl *x509.Certificate) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	for _, name := range []string{certName, keyName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return "", fmt.Errorf("%s already exists in %s", name, dir)
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", err
	}
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().AddDate(20, 0, 0)
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}

	// The key is written first, so a failure can't leave a certificate without one.
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := writeNew(filepath.Join(dir, keyName), keyPEM, 0o600); err != nil {
		return "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeNew(filepath.Join(dir, certName), certPEM, 0o644); err != nil {
		return "", err
	}
	return Fingerprint(der), nil
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Fingerprint is the SHA-256 of a DER certificate, in hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func certFingerprint(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s isn't a certificate", path)
	}
	return Fingerprint(block.Bytes), nil
}

// ServerFingerprint reads the server's public fingerprint.
func ServerFingerprint(dir string) (string, error) {
	return certFingerprint(filepath.Join(dir, serverCert))
}

// ClientFingerprint reads a client's public fingerprint.
func ClientFingerprint(dir string) (string, error) {
	return certFingerprint(filepath.Join(dir, clientCert))
}

// Client is one entry in the allow list.
type Client struct {
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
}

// Clients reads the allow list. A missing file means nobody is allowed.
func Clients(dir string) ([]Client, error) {
	f, err := os.Open(filepath.Join(dir, allowFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Client
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fingerprint, name, _ := strings.Cut(strings.TrimSpace(scanner.Text()), " ")
		if fingerprint == "" {
			continue
		}
		if !fingerprintRE.MatchString(fingerprint) {
			return nil, fmt.Errorf("%s has a malformed line", allowFile)
		}
		out = append(out, Client{Fingerprint: fingerprint, Name: name})
	}
	return out, scanner.Err()
}

func writeClients(dir string, clients []Client) error {
	var b strings.Builder
	for _, c := range clients {
		fmt.Fprintf(&b, "%s %s\n", c.Fingerprint, c.Name)
	}
	tmp, err := os.CreateTemp(dir, ".clients-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(b.String())
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, allowFile))
}

// Allow adds a client. Allowing one again just updates its name.
func Allow(dir, fingerprint, name string) error {
	fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	if !fingerprintRE.MatchString(fingerprint) {
		return errors.New("a fingerprint must be 64 hex characters")
	}
	name = strings.Join(strings.Fields(name), "-")
	clients, err := Clients(dir)
	if err != nil {
		return err
	}
	clients = slices.DeleteFunc(clients, func(c Client) bool { return c.Fingerprint == fingerprint })
	return writeClients(dir, append(clients, Client{Fingerprint: fingerprint, Name: name}))
}

// Revoke removes a client.
func Revoke(dir, fingerprint string) error {
	fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	clients, err := Clients(dir)
	if err != nil {
		return err
	}
	kept := slices.DeleteFunc(clients, func(c Client) bool { return c.Fingerprint == fingerprint })
	if len(kept) == len(clients) {
		return errors.New("that client isn't on the allow list")
	}
	return writeClients(dir, kept)
}

// Allowed reports whether a client may connect. The list is read every time so
// changes apply without a restart, and any error denies access.
func Allowed(dir, fingerprint string) bool {
	clients, err := Clients(dir)
	if err != nil {
		return false
	}
	found := false
	for _, c := range clients {
		if subtle.ConstantTimeCompare([]byte(c.Fingerprint), []byte(fingerprint)) == 1 {
			found = true
		}
	}
	return found
}

// ServerTLS loads the server's identity and requires an allowed client
// certificate on every connection. Holding the private key is proved by the TLS
// handshake itself. Session tickets are off so each connection does this in full.
func ServerTLS(dir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, serverCert), filepath.Join(dir, serverKey))
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:           []tls.Certificate{cert},
		MinVersion:             tls.VersionTLS13,
		ClientAuth:             tls.RequireAnyClientCert,
		SessionTicketsDisabled: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("client sent no certificate")
			}
			got := Fingerprint(cs.PeerCertificates[0].Raw)
			if !Allowed(dir, got) {
				return fmt.Errorf("client certificate %s isn't on the allow list", got)
			}
			return nil
		},
	}, nil
}

// LoadClientCert loads the client's identity for use in ClientTLS.
func LoadClientCert(dir string) (tls.Certificate, error) {
	return tls.LoadX509KeyPair(filepath.Join(dir, clientCert), filepath.Join(dir, clientKey))
}

// ClientTLS trusts exactly one server certificate, the one saved at login.
// Normal chain and hostname checks don't apply to a self-signed certificate, so
// they're replaced by this stricter check.
func ClientTLS(serverFingerprint string, cert tls.Certificate) *tls.Config {
	want := strings.ToLower(serverFingerprint)
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &cert, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server sent no certificate")
			}
			got := Fingerprint(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return fmt.Errorf("server certificate %s doesn't match the saved fingerprint", got)
			}
			return nil
		},
	}
}
