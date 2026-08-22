package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

var oidIssuerV2 = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}

func main() {
	output := flag.String("output", "", "directory for generated test material")
	publicKeyPath := flag.String("public-key", "", "Cosign public key in PEM format")
	identity := flag.String("identity", "", "URI SAN identity")
	issuer := flag.String("issuer", "", "Fulcio OIDC issuer extension")
	flag.Parse()
	if *output == "" || *publicKeyPath == "" || *identity == "" || *issuer == "" {
		fatalf("--output, --public-key, --identity, and --issuer are required")
	}
	identityURI, err := url.ParseRequestURI(*identity)
	if err != nil || identityURI.Scheme != "https" || identityURI.Host == "" {
		fatalf("identity must be an absolute HTTPS URI")
	}
	if err := os.MkdirAll(*output, 0o700); err != nil {
		fatalf("create output: %v", err)
	}

	now := time.Now().UTC()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fatalf("generate root key: %v", err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{Organization: []string{"Crabbox Test"}, CommonName: "Crabbox Test Fulcio Root"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		fatalf("create root certificate: %v", err)
	}

	leafPublicKey, err := readPublicKey(*publicKeyPath)
	if err != nil {
		fatalf("read leaf public key: %v", err)
	}
	issuerDER, err := asn1.MarshalWithParams(*issuer, "utf8")
	if err != nil {
		fatalf("encode issuer: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{Organization: []string{"Crabbox Test"}, CommonName: "Crabbox Test Signer"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{identityURI},
		ExtraExtensions: []pkix.Extension{
			{Id: oidIssuerV2, Value: issuerDER},
		},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, rootTemplate, leafPublicKey, rootKey)
	if err != nil {
		fatalf("create leaf certificate: %v", err)
	}

	writePEM(filepath.Join(*output, "cosign.crt"), "CERTIFICATE", leafDER, 0o644)
	writePEM(filepath.Join(*output, "chain.crt"), "CERTIFICATE", rootDER, 0o644)

	trustedRoot := map[string]any{
		"mediaType": "application/vnd.dev.sigstore.trustedroot+json;version=0.1",
		"certificateAuthorities": []any{
			map[string]any{
				"uri": "https://fulcio.test.invalid",
				"subject": map[string]any{
					"organization": "Crabbox Test",
					"commonName":   "Crabbox Test Fulcio Root",
				},
				"certChain": map[string]any{
					"certificates": []any{
						map[string]any{"rawBytes": base64.StdEncoding.EncodeToString(rootDER)},
					},
				},
				"validFor": map[string]any{
					"start": rootTemplate.NotBefore.Format(time.RFC3339),
					"end":   rootTemplate.NotAfter.Format(time.RFC3339),
				},
			},
		},
	}
	trustedRootBytes, err := json.Marshal(trustedRoot)
	if err != nil {
		fatalf("marshal trusted root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(*output, "trusted-root.json"), append(trustedRootBytes, '\n'), 0o644); err != nil {
		fatalf("write trusted root: %v", err)
	}
}

func readPublicKey(path string) (crypto.PublicKey, error) {
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(bytes)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("expected PUBLIC KEY PEM")
	}
	return x509.ParsePKIXPublicKey(block.Bytes)
}

func serial() *big.Int {
	value, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		fatalf("generate serial: %v", err)
	}
	return value
}

func writePEM(path, blockType string, bytes []byte, mode os.FileMode) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		fatalf("create %s: %v", path, err)
	}
	if err := pem.Encode(file, &pem.Block{Type: blockType, Bytes: bytes}); err != nil {
		_ = file.Close()
		fatalf("encode %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		fatalf("close %s: %v", path, err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
