package main

// TLS for the API: serve takes a certificate and key (re-read whenever the
// files change, so a renewal needs no restart), `imgrec cert` makes a
// self-signed pair for a network with no CA, and submit trusts one with
// --cacert.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// certReloader serves the certificate in two files, reloading them when
// either changes. A pair that fails to load (half-written by a renewal)
// keeps the previous one in service.
type certReloader struct {
	certFile, keyFile string
	logf              func(format string, a ...any)

	mu    sync.Mutex
	cert  *tls.Certificate
	stamp string
}

func newCertReloader(certFile, keyFile string, logf func(string, ...any)) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, logf: logf}
	if _, err := r.get(nil); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certReloader) fileStamp() string {
	var parts []string
	for _, f := range []string{r.certFile, r.keyFile} {
		st, err := os.Stat(f)
		if err != nil {
			return ""
		}
		parts = append(parts, fmt.Sprintf("%d/%d", st.ModTime().UnixNano(), st.Size()))
	}
	return strings.Join(parts, " ")
}

func (r *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stamp := r.fileStamp()
	if r.cert != nil && (stamp == r.stamp || stamp == "") {
		return r.cert, nil
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if r.cert != nil {
			if r.logf != nil {
				r.logf("tls: %s: %v; keeping the certificate already loaded", r.certFile, err)
			}
			return r.cert, nil
		}
		return nil, fmt.Errorf("tls: %w", err)
	}
	if r.cert != nil && r.logf != nil {
		r.logf("tls: certificate reloaded (%s)", fingerprint(c.Certificate[0]))
	}
	r.cert, r.stamp = &c, stamp
	return r.cert, nil
}

func (r *certReloader) config() *tls.Config {
	return &tls.Config{GetCertificate: r.get, MinVersion: tls.VersionTLS12}
}

func fingerprint(der []byte) string {
	h := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(h[:])
}

// selfSigned makes a certificate that is its own CA: give it to clients as
// --cacert and it is the only thing they need to trust.
func selfSigned(hosts []string, days int) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: hosts[0], Organization: []string{"imgrec"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(0, 0, days),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), nil
}

// imgrec cert: a self-signed certificate and key for serve --tls-cert/--tls-key.
func certCmd(args []string) error {
	fs := flag.NewFlagSet("cert", flag.ExitOnError)
	hosts := fs.String("hosts", "", "comma-separated names and addresses the server is reached by (required)")
	certOut := fs.String("cert", "imgrec.crt", "where to write the certificate (give it to clients as --cacert)")
	keyOut := fs.String("key", "imgrec.key", "where to write the private key")
	days := fs.Int("days", 3650, "validity, in days")
	ifMissing := fs.Bool("if-missing", false, "do nothing if the certificate already exists")
	fs.Parse(args)
	if *ifMissing {
		if _, err := os.Stat(*certOut); err == nil {
			return nil
		}
	}
	var hs []string
	for _, h := range strings.Split(*hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hs = append(hs, h)
		}
	}
	if len(hs) == 0 {
		return errors.New("usage: imgrec cert --hosts name[,name,ip...] [--cert imgrec.crt] [--key imgrec.key]")
	}
	certPEM, keyPEM, err := selfSigned(hs, *days)
	if err != nil {
		return err
	}
	// the key first, never readable by others; neither is overwritten
	if err := writeNew(*keyOut, keyPEM, 0o600); err != nil {
		return err
	}
	if err := writeNew(*certOut, certPEM, 0o644); err != nil {
		os.Remove(*keyOut)
		return err
	}
	block, _ := pem.Decode(certPEM)
	fmt.Fprintf(os.Stderr, "imgrec: wrote %s and %s for %s (%s)\n", *certOut, *keyOut,
		strings.Join(hs, ", "), fingerprint(block.Bytes))
	return nil
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// clientTLS is the client side: the system's roots, plus cacert if given.
func clientTLS(cacert string) (*tls.Config, error) {
	if cacert == "" {
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	data, err := os.ReadFile(cacert)
	if err != nil {
		return nil, err
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%s: no PEM certificate in it", cacert)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// cleartextRemote is true for an http:// server that isn't this machine:
// the token would cross the network in the clear.
func cleartextRemote(server string) bool {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}
