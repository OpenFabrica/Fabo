//go:build linux || darwin

package localengine

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

func domainTLS(dir string) (*tls.Config, error) {
	ca, err := domainCA(dir)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	cache := make(map[string]*tls.Certificate)
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			host, name := domainHost(hello.ServerName)
			if name == "" || host != strings.ToLower(strings.TrimSuffix(hello.ServerName, ".")) {
				return nil, errors.New("unknown container domain")
			}
			mu.Lock()
			defer mu.Unlock()
			now := time.Now()
			if cert := cache[host]; cert != nil && cert.Leaf.NotAfter.After(now.Add(24*time.Hour)) {
				return cert, nil
			}
			leaf := &x509.Certificate{
				DNSNames: []string{host}, NotBefore: now.Add(-time.Hour), NotAfter: minTime(now.AddDate(0, 1, 0), ca.Leaf.NotAfter),
				KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}
			cert, err := makeDomainCert(leaf, ca.Leaf, ca.PrivateKey)
			if err != nil {
				return nil, err
			}
			if len(cache) >= 256 {
				clear(cache)
			}
			cache[host] = cert
			return cert, nil
		},
	}, nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func makeDomainCert(cert, issuer *x509.Certificate, signer any) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	if signer == nil {
		signer = key
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, issuer, key.Public(), signer)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

func domainCA(dir string) (*tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	versionPath := filepath.Join(dir, "ca.version")
	version, err := os.ReadFile(versionPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if strings.TrimSpace(string(version)) == "2" {
		ca, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, err
		}
		excludesAll := func(size int) bool {
			return slices.ContainsFunc(ca.Leaf.ExcludedIPRanges, func(network *net.IPNet) bool {
				ones, bits := network.Mask.Size()
				return ones == 0 && bits == size
			})
		}
		if !ca.Leaf.IsCA || !slices.Equal(ca.Leaf.PermittedDNSDomains, domainSuffixes) || !excludesAll(32) || !excludesAll(128) {
			return nil, errors.New("container-domain authority has invalid constraints")
		}
		return &ca, nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	now := time.Now()
	root := &x509.Certificate{
		Subject:   pkix.Name{CommonName: "Kiri Local Certificate Authority", Organization: []string{"Kiri"}},
		NotBefore: now.Add(-24 * time.Hour), NotAfter: now.AddDate(20, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		PermittedDNSDomainsCritical: true, PermittedDNSDomains: domainSuffixes,
		ExcludedIPRanges: []*net.IPNet{
			{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		},
	}
	ca, err := makeDomainCert(root, root, nil)
	if err != nil {
		return nil, err
	}
	key, err := x509.MarshalPKCS8PrivateKey(ca.PrivateKey)
	if err != nil {
		return nil, err
	}
	for _, file := range []struct {
		path string
		data []byte
	}{
		{keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})},
		{certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate[0]})},
		{versionPath, []byte("2")},
	} {
		tmp := file.path + ".tmp"
		if err := os.WriteFile(tmp, file.data, 0600); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, file.path); err != nil {
			return nil, fmt.Errorf("save container-domain authority: %w", err)
		}
	}
	return ca, nil
}
