package workergateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	IssuerProviderVault = "vault"
	maxVaultResponse    = 2 << 20
	maxVaultTokenBytes  = 16 << 10
)

var vaultPathSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type VaultIssuerConfig struct {
	ID           string
	Address      string
	Mount        string
	Role         string
	TokenPath    string
	Namespace    string
	ServerCAPath string
	Timeout      time.Duration
}

// VaultIssuer submits a locally generated CSR to a narrowly scoped Vault PKI
// sign role. The CA private key never enters the Portage Engine process.
type VaultIssuer struct {
	config VaultIssuerConfig
}

func NewVaultIssuer(config VaultIssuerConfig) (*VaultIssuer, error) {
	config.ID = strings.TrimSpace(config.ID)
	config.Address = strings.TrimRight(strings.TrimSpace(config.Address), "/")
	config.Mount = strings.TrimSpace(config.Mount)
	config.Role = strings.TrimSpace(config.Role)
	config.TokenPath = strings.TrimSpace(config.TokenPath)
	config.Namespace = strings.TrimSpace(config.Namespace)
	config.ServerCAPath = strings.TrimSpace(config.ServerCAPath)
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Second
	}
	if config.ID == "" || len(config.ID) > 128 {
		return nil, fmt.Errorf("Vault worker issuer ID must contain 1..128 characters")
	}
	address, err := url.Parse(config.Address)
	if err != nil || address.Scheme != "https" || address.Host == "" ||
		address.User != nil || address.RawQuery != "" || address.Fragment != "" ||
		(address.Path != "" && address.Path != "/") {
		return nil, fmt.Errorf("Vault address must be an HTTPS origin")
	}
	if !vaultPathSegment.MatchString(config.Mount) ||
		!vaultPathSegment.MatchString(config.Role) {
		return nil, fmt.Errorf("Vault PKI mount and role must be single safe path segments")
	}
	if config.TokenPath == "" {
		return nil, fmt.Errorf("Vault token file is required")
	}
	if len(config.Namespace) > 256 ||
		strings.ContainsAny(config.Namespace, "\r\n") {
		return nil, fmt.Errorf("Vault namespace is invalid")
	}
	if config.Timeout > time.Minute {
		return nil, fmt.Errorf("Vault request timeout must not exceed one minute")
	}
	return &VaultIssuer{config: config}, nil
}

func (i *VaultIssuer) Provider() string { return IssuerProviderVault }
func (i *VaultIssuer) ID() string       { return i.config.ID }

func (i *VaultIssuer) Issue(
	ctx context.Context,
	identity Identity,
	ttl time.Duration,
) (IssuedCertificate, error) {
	if i == nil {
		return IssuedCertificate{}, fmt.Errorf("Vault worker issuer is not configured")
	}
	if err := identity.Validate(); err != nil {
		return IssuedCertificate{}, err
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return IssuedCertificate{}, fmt.Errorf("worker certificate TTL must be in (0, 24h]")
	}
	identityURI, err := identity.URI()
	if err != nil {
		return IssuedCertificate{}, err
	}
	publicKey, privateKey, csrPEM, err := workerCSR(identityURI)
	if err != nil {
		return IssuedCertificate{}, err
	}
	token, err := readOwnerOnlySecret(i.config.TokenPath, maxVaultTokenBytes)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("read Vault token: %w", err)
	}
	defer clear(token)
	response, err := i.signCSR(ctx, token, csrPEM, identityURI.String(), ttl)
	if err != nil {
		return IssuedCertificate{}, err
	}
	issued, err := i.validateResponse(response, identity, publicKey, ttl)
	if err != nil {
		return IssuedCertificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("marshal worker key: %w", err)
	}
	defer clear(keyDER)
	issued.KeyPEM = pem.EncodeToMemory(
		&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER},
	)
	return issued, nil
}

func workerCSR(
	identityURI *url.URL,
) (ed25519.PublicKey, ed25519.PrivateKey, []byte, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate worker key: %w", err)
	}
	der, err := x509.CreateCertificateRequest(
		rand.Reader,
		&x509.CertificateRequest{
			Subject: pkix.Name{CommonName: "portage-worker"},
			URIs:    []*url.URL{identityURI},
		},
		privateKey,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create worker CSR: %w", err)
	}
	return publicKey, privateKey, pem.EncodeToMemory(
		&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der},
	), nil
}

type vaultSignResponse struct {
	Data struct {
		Certificate string   `json:"certificate"`
		IssuingCA   string   `json:"issuing_ca"`
		CAChain     []string `json:"ca_chain"`
	} `json:"data"`
	Errors []string `json:"errors"`
}

func (i *VaultIssuer) signCSR(
	ctx context.Context,
	token, csrPEM []byte,
	identityURI string,
	ttl time.Duration,
) (vaultSignResponse, error) {
	payload, err := json.Marshal(map[string]any{
		"csr":                  string(csrPEM),
		"common_name":          "portage-worker",
		"uri_sans":             identityURI,
		"ttl":                  ttl.String(),
		"format":               "pem",
		"exclude_cn_from_sans": true,
	})
	if err != nil {
		return vaultSignResponse{}, fmt.Errorf("encode Vault sign request: %w", err)
	}
	endpoint := i.config.Address + "/v1/" + i.config.Mount +
		"/sign/" + i.config.Role
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint, bytes.NewReader(payload),
	)
	if err != nil {
		return vaultSignResponse{}, fmt.Errorf("create Vault sign request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Vault-Token", string(token))
	if i.config.Namespace != "" {
		request.Header.Set("X-Vault-Namespace", i.config.Namespace)
	}
	client, err := i.httpClient()
	if err != nil {
		return vaultSignResponse{}, err
	}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	// Drop the request's reference to the bearer string as soon as the
	// transport has cloned and sent the headers.
	request.Header.Del("X-Vault-Token")
	if err != nil {
		return vaultSignResponse{}, fmt.Errorf("Vault PKI sign request failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxVaultResponse+1))
	if err != nil {
		return vaultSignResponse{}, fmt.Errorf("read Vault PKI response: %w", err)
	}
	if len(body) > maxVaultResponse {
		return vaultSignResponse{}, fmt.Errorf("Vault PKI response exceeds size limit")
	}
	var decoded vaultSignResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return vaultSignResponse{}, fmt.Errorf(
			"decode Vault PKI response (status %d): %w",
			response.StatusCode, err,
		)
	}
	if response.StatusCode != http.StatusOK {
		message := strings.Join(decoded.Errors, "; ")
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		if len(message) > 4096 {
			message = message[:4096]
		}
		return vaultSignResponse{}, fmt.Errorf(
			"Vault PKI sign rejected (status %d): %s",
			response.StatusCode, message,
		)
	}
	return decoded, nil
}

func (i *VaultIssuer) httpClient() (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system roots for Vault: %w", err)
	}
	if roots == nil {
		roots = x509.NewCertPool()
	}
	if i.config.ServerCAPath != "" {
		caPEM, err := os.ReadFile(i.config.ServerCAPath)
		if err != nil {
			return nil, fmt.Errorf("read Vault server CA: %w", err)
		}
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("parse Vault server CA")
		}
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			// Vault commonly sits behind enterprise TLS terminators where
			// TLS 1.2 remains the minimum supported version.
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		},
		Proxy:               nil,
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        2,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}
	return &http.Client{
		Timeout:   i.config.Timeout,
		Transport: transport,
		CheckRedirect: func(
			_ *http.Request,
			_ []*http.Request,
		) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

func (i *VaultIssuer) validateResponse(
	response vaultSignResponse,
	identity Identity,
	expectedPublicKey ed25519.PublicKey,
	ttl time.Duration,
) (IssuedCertificate, error) {
	leafChain, err := parseCertificatePEMChain(
		[]byte(response.Data.Certificate),
	)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("parse Vault worker certificate: %w", err)
	}
	leaf := leafChain[0]
	if err := verifyWorkerLeaf(leaf, identity, expectedPublicKey); err != nil {
		return IssuedCertificate{}, err
	}
	issuer, chain, err := vaultIssuerChain(leaf, response)
	if err != nil {
		return IssuedCertificate{}, err
	}
	now := time.Now().UTC()
	if now.Before(leaf.NotBefore.Add(-2*time.Minute)) ||
		!now.Before(leaf.NotAfter) ||
		leaf.NotAfter.After(now.Add(ttl+2*time.Minute)) {
		return IssuedCertificate{}, fmt.Errorf(
			"Vault worker certificate validity is outside the requested TTL",
		)
	}
	record := certificateRecord(
		leaf, issuer, leaf.Raw, i.config.ID, i.Provider(),
	)
	if err := record.Validate(); err != nil {
		return IssuedCertificate{}, err
	}
	return IssuedCertificate{
		CertPEM: encodeCertificateChain(leaf, chain),
		Record:  record,
	}, nil
}

func vaultIssuerChain(
	leaf *x509.Certificate,
	response vaultSignResponse,
) (*x509.Certificate, []*x509.Certificate, error) {
	var candidates []*x509.Certificate
	for _, encoded := range append(
		[]string{response.Data.IssuingCA},
		response.Data.CAChain...,
	) {
		if strings.TrimSpace(encoded) == "" {
			continue
		}
		parsed, err := parseCertificatePEMChain([]byte(encoded))
		if err != nil {
			return nil, nil, fmt.Errorf("parse Vault issuer chain: %w", err)
		}
		for _, certificate := range parsed {
			if !slices.ContainsFunc(candidates, func(existing *x509.Certificate) bool {
				return existing.Equal(certificate)
			}) {
				candidates = append(candidates, certificate)
			}
		}
	}
	for _, candidate := range candidates {
		if candidate.IsCA && leaf.CheckSignatureFrom(candidate) == nil {
			return candidate, candidates, nil
		}
	}
	return nil, nil, fmt.Errorf("Vault response does not contain the direct issuing CA")
}

func encodeCertificateChain(
	leaf *x509.Certificate,
	chain []*x509.Certificate,
) []byte {
	encoded := pem.EncodeToMemory(
		&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw},
	)
	for _, certificate := range chain {
		encoded = append(encoded, pem.EncodeToMemory(
			&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw},
		)...)
	}
	return encoded
}

func readOwnerOnlySecret(path string, limit int64) ([]byte, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("secret file must not be a symbolic link")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("secret file must be regular and unreadable by group or others")
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > limit {
		clear(value)
		return nil, fmt.Errorf("secret file exceeds size limit")
	}
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		clear(value)
		return nil, fmt.Errorf("secret file is empty")
	}
	return value, nil
}
