package workergateway

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	IssuerProviderFile = "file"
	fingerprintLength  = 64
)

// CertificateRecord is the non-secret, durable identity of one issued leaf.
// Neither the leaf PEM nor its private key is persisted in PostgreSQL.
type CertificateRecord struct {
	Fingerprint       string    `json:"fingerprint"`
	Serial            string    `json:"serial"`
	IssuerID          string    `json:"issuer_id"`
	IssuerProvider    string    `json:"issuer_provider"`
	IssuerFingerprint string    `json:"issuer_fingerprint"`
	IssuerSubject     string    `json:"issuer_subject"`
	IssuerSerial      string    `json:"issuer_serial"`
	IssuerNotBefore   time.Time `json:"issuer_not_before"`
	IssuerNotAfter    time.Time `json:"issuer_not_after"`
	NotBefore         time.Time `json:"not_before"`
	NotAfter          time.Time `json:"not_after"`
}

func (c CertificateRecord) Validate() error {
	for name, value := range map[string]string{
		"certificate fingerprint": c.Fingerprint,
		"issuer fingerprint":      c.IssuerFingerprint,
	} {
		if len(value) != fingerprintLength {
			return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
		}
		if _, err := hex.DecodeString(value); err != nil || value != strings.ToLower(value) {
			return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
		}
	}
	for name, value := range map[string]string{
		"certificate serial": c.Serial,
		"issuer serial":      c.IssuerSerial,
	} {
		if value == "" || len(value) > 256 ||
			value != strings.ToLower(value) {
			return fmt.Errorf("%s must be lowercase hexadecimal", name)
		}
		if _, err := hex.DecodeString(padHex(value)); err != nil {
			return fmt.Errorf("%s must be lowercase hexadecimal", name)
		}
	}
	if strings.TrimSpace(c.Serial) == "" || strings.TrimSpace(c.IssuerSerial) == "" ||
		strings.TrimSpace(c.IssuerID) == "" || strings.TrimSpace(c.IssuerProvider) == "" ||
		c.NotBefore.IsZero() || !c.NotAfter.After(c.NotBefore) ||
		c.IssuerNotBefore.IsZero() || !c.IssuerNotAfter.After(c.IssuerNotBefore) ||
		c.NotAfter.After(c.IssuerNotAfter) {
		return fmt.Errorf("certificate record is incomplete")
	}
	return nil
}

func padHex(value string) string {
	if len(value)%2 == 0 {
		return value
	}
	return "0" + value
}

// PresentedCertificate is recalculated from the verified TLS peer leaf on
// every request. It is sufficient to bind the request to the durable record.
type PresentedCertificate struct {
	Fingerprint string
	Serial      string
}

// IssuerGenerationStatus and CertificateStatus are redacted operator views.
type IssuerGenerationStatus struct {
	Fingerprint        string     `json:"fingerprint"`
	IssuerID           string     `json:"issuer_id"`
	Provider           string     `json:"provider"`
	Subject            string     `json:"subject"`
	Serial             string     `json:"serial"`
	NotBefore          time.Time  `json:"not_before"`
	NotAfter           time.Time  `json:"not_after"`
	State              string     `json:"state"`
	LastIssuedAt       time.Time  `json:"last_issued_at"`
	ActiveCertificates int        `json:"active_certificates"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
	RevokeReason       string     `json:"revoke_reason,omitempty"`
}

type CertificateStatus struct {
	Fingerprint       string     `json:"fingerprint"`
	Serial            string     `json:"serial"`
	IssuerFingerprint string     `json:"issuer_fingerprint"`
	WorkerID          string     `json:"worker_id"`
	JobID             string     `json:"job_id"`
	AttemptID         string     `json:"attempt_id"`
	AttemptFence      int64      `json:"attempt_fence"`
	NotBefore         time.Time  `json:"not_before"`
	NotAfter          time.Time  `json:"not_after"`
	State             string     `json:"state"`
	IssuedAt          time.Time  `json:"issued_at"`
	LastSeenAt        *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	RevokeReason      string     `json:"revoke_reason,omitempty"`
}

func CertificatePresentation(cert *x509.Certificate) (PresentedCertificate, error) {
	if cert == nil || len(cert.Raw) == 0 || cert.SerialNumber == nil {
		return PresentedCertificate{}, fmt.Errorf("worker certificate metadata is incomplete")
	}
	return PresentedCertificate{
		Fingerprint: certificateFingerprint(cert.Raw),
		Serial:      strings.ToLower(cert.SerialNumber.Text(16)),
	}, nil
}

// IssuedCertificate contains the one-shot bootstrap material plus its durable
// public metadata. Callers must never log or persist KeyPEM.
type IssuedCertificate struct {
	CertPEM []byte
	KeyPEM  []byte
	Record  CertificateRecord
}

// Issuer is the provider boundary for workload certificate issuance. External
// providers (for example step-ca, Vault PKI, or SPIRE) can implement this
// contract without exposing their signing key to the control plane.
type Issuer interface {
	Issue(context.Context, Identity, time.Duration) (IssuedCertificate, error)
	Provider() string
	ID() string
}

// TrustingIssuer verifies every newly issued leaf against the exact immutable
// trust roots loaded by the current Gateway listener. This prevents a remote
// issuer rotation from handing workers a certificate that another replica (or
// the current listener before its trust-bundle restart) cannot authenticate.
type TrustingIssuer struct {
	delegate Issuer
	roots    *x509.CertPool
	mu       sync.RWMutex
	status   IssuerRuntimeStatus
}

func NewTrustingIssuer(delegate Issuer, roots *x509.CertPool) (*TrustingIssuer, error) {
	if delegate == nil || roots == nil {
		return nil, fmt.Errorf("worker issuer and listener trust roots are required")
	}
	return &TrustingIssuer{
		delegate: delegate,
		roots:    roots,
		status: IssuerRuntimeStatus{
			ID: delegate.ID(), Provider: delegate.Provider(),
		},
	}, nil
}

func (i *TrustingIssuer) Provider() string { return i.delegate.Provider() }
func (i *TrustingIssuer) ID() string       { return i.delegate.ID() }

func (i *TrustingIssuer) Issue(
	ctx context.Context,
	identity Identity,
	ttl time.Duration,
) (IssuedCertificate, error) {
	issued, err := i.delegate.Issue(ctx, identity, ttl)
	if err != nil {
		i.recordResult(err)
		return IssuedCertificate{}, err
	}
	if err := verifyIssuedCertificateTrust(issued, identity, i.roots); err != nil {
		clear(issued.KeyPEM)
		i.recordResult(err)
		return IssuedCertificate{}, err
	}
	i.recordResult(nil)
	return issued, nil
}

type IssuerRuntimeStatus struct {
	ID                  string     `json:"id"`
	Provider            string     `json:"provider"`
	Healthy             bool       `json:"healthy"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastFailureAt       *time.Time `json:"last_failure_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
}

func (i *TrustingIssuer) Status() IssuerRuntimeStatus {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.status
}

func (i *TrustingIssuer) recordResult(result error) {
	now := time.Now().UTC()
	i.mu.Lock()
	defer i.mu.Unlock()
	if result == nil {
		i.status.Healthy = true
		i.status.ConsecutiveFailures = 0
		i.status.LastSuccessAt = &now
		i.status.LastError = ""
		return
	}
	i.status.Healthy = false
	i.status.ConsecutiveFailures++
	i.status.LastFailureAt = &now
	i.status.LastError = result.Error()
	if len(i.status.LastError) > 1024 {
		i.status.LastError = i.status.LastError[:1024]
	}
}

func IssuerStatus(issuer Issuer) IssuerRuntimeStatus {
	if issuer == nil {
		return IssuerRuntimeStatus{}
	}
	if statusIssuer, ok := issuer.(interface {
		Status() IssuerRuntimeStatus
	}); ok {
		return statusIssuer.Status()
	}
	return IssuerRuntimeStatus{
		ID: issuer.ID(), Provider: issuer.Provider(), Healthy: true,
	}
}

// FileIssuer is the bootstrap/reference provider. It reloads the certificate
// and key for every issuance so a staged file rotation takes effect without
// retaining private-key bytes in process-global state.
type FileIssuer struct {
	id       string
	certPath string
	keyPath  string
}

func NewFileIssuer(id, certPath, keyPath string) *FileIssuer {
	return &FileIssuer{
		id: strings.TrimSpace(id), certPath: certPath, keyPath: keyPath,
	}
}

func (i *FileIssuer) Provider() string { return IssuerProviderFile }
func (i *FileIssuer) ID() string       { return i.id }

func (i *FileIssuer) Issue(
	_ context.Context,
	identity Identity,
	ttl time.Duration,
) (IssuedCertificate, error) {
	if i == nil || i.id == "" || i.certPath == "" || i.keyPath == "" {
		return IssuedCertificate{}, fmt.Errorf("file worker issuer is not configured")
	}
	certPEM, err := os.ReadFile(i.certPath)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("read worker issuer certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(i.keyPath)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("read worker issuer key: %w", err)
	}
	defer clear(keyPEM)
	keyInfo, err := os.Stat(i.keyPath)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("stat worker issuer key: %w", err)
	}
	if keyInfo.Mode().Perm()&0o077 != 0 {
		return IssuedCertificate{}, fmt.Errorf("worker issuer key must not be readable by group or others")
	}
	return issueWorkerCertificate(certPEM, keyPEM, identity, ttl, i.id, i.Provider())
}

// IssueWorkerCertificate creates a short-lived client certificate bound to one
// scheduler identity. It is retained as a compatibility helper for tests and
// embedders; production code should use an Issuer and persist Record.
func IssueWorkerCertificate(
	caCertPEM, caKeyPEM []byte,
	identity Identity,
	ttl time.Duration,
) (certPEM, keyPEM []byte, err error) {
	issued, err := issueWorkerCertificate(
		caCertPEM, caKeyPEM, identity, ttl, "compatibility-file",
		IssuerProviderFile,
	)
	if err != nil {
		return nil, nil, err
	}
	return issued.CertPEM, issued.KeyPEM, nil
}

func issueWorkerCertificate(
	caCertPEM, caKeyPEM []byte,
	identity Identity,
	ttl time.Duration,
	issuerID, issuerProvider string,
) (IssuedCertificate, error) {
	if err := identity.Validate(); err != nil {
		return IssuedCertificate{}, err
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return IssuedCertificate{}, fmt.Errorf("worker certificate TTL must be in (0, 24h]")
	}
	if strings.TrimSpace(issuerID) == "" || strings.TrimSpace(issuerProvider) == "" {
		return IssuedCertificate{}, fmt.Errorf("worker issuer identity is incomplete")
	}
	caCert, caKey, err := parseCertificateAuthority(caCertPEM, caKeyPEM)
	if err != nil {
		return IssuedCertificate{}, err
	}
	identityURI, err := identity.URI()
	if err != nil {
		return IssuedCertificate{}, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("generate worker certificate serial: %w", err)
	}
	workerPublic, workerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("generate worker key: %w", err)
	}
	now := time.Now().UTC()
	if err := validateIssuerWindow(caCert, now, ttl); err != nil {
		return IssuedCertificate{}, err
	}
	notBefore := now.Add(-time.Minute)
	if notBefore.Before(caCert.NotBefore) {
		notBefore = caCert.NotBefore
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      caCert.Subject,
		NotBefore:    notBefore,
		NotAfter:     now.Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{identityURI},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, workerPublic, caKey)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("issue worker certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(workerPrivate)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("marshal worker key: %w", err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	leafPEM = append(leafPEM, caCertPEM...)
	record := certificateRecord(
		template, caCert, der, issuerID, issuerProvider,
	)
	if err := record.Validate(); err != nil {
		return IssuedCertificate{}, err
	}
	return IssuedCertificate{
		CertPEM: leafPEM,
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		Record:  record,
	}, nil
}

func certificateRecord(
	leaf, issuer *x509.Certificate,
	leafDER []byte,
	issuerID, issuerProvider string,
) CertificateRecord {
	return CertificateRecord{
		Fingerprint:       certificateFingerprint(leafDER),
		Serial:            strings.ToLower(leaf.SerialNumber.Text(16)),
		IssuerID:          strings.TrimSpace(issuerID),
		IssuerProvider:    strings.TrimSpace(issuerProvider),
		IssuerFingerprint: certificateFingerprint(issuer.Raw),
		IssuerSubject:     issuer.Subject.String(),
		IssuerSerial:      strings.ToLower(issuer.SerialNumber.Text(16)),
		IssuerNotBefore:   issuer.NotBefore.UTC(),
		IssuerNotAfter:    issuer.NotAfter.UTC(),
		NotBefore:         leaf.NotBefore.UTC(),
		NotAfter:          leaf.NotAfter.UTC(),
	}
}

func verifyIssuedCertificateTrust(
	issued IssuedCertificate,
	identity Identity,
	roots *x509.CertPool,
) error {
	certificates, err := parseCertificatePEMChain(issued.CertPEM)
	if err != nil {
		return fmt.Errorf("parse issued worker certificate chain: %w", err)
	}
	leaf := certificates[0]
	intermediates := x509.NewCertPool()
	for _, certificate := range certificates[1:] {
		intermediates.AddCert(certificate)
	}
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: intermediates,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return fmt.Errorf("issued worker certificate is outside listener trust: %w", err)
	}
	if len(chains) == 0 || len(chains[0]) < 2 {
		return fmt.Errorf("issued worker certificate has no verified issuer")
	}
	if err := verifyWorkerLeaf(leaf, identity, nil); err != nil {
		return err
	}
	presented, err := CertificatePresentation(leaf)
	if err != nil {
		return err
	}
	if presented.Fingerprint != issued.Record.Fingerprint ||
		presented.Serial != issued.Record.Serial ||
		certificateFingerprint(chains[0][1].Raw) !=
			issued.Record.IssuerFingerprint {
		return fmt.Errorf("issued worker certificate metadata does not match its verified chain")
	}
	return nil
}

func parseCertificatePEMChain(encoded []byte) ([]*x509.Certificate, error) {
	var certificates []*x509.Certificate
	for len(encoded) > 0 {
		block, rest := pem.Decode(encoded)
		if block == nil {
			break
		}
		encoded = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, certificate)
	}
	if len(certificates) == 0 {
		return nil, fmt.Errorf("certificate PEM is empty")
	}
	return certificates, nil
}

func verifyWorkerLeaf(
	leaf *x509.Certificate,
	identity Identity,
	expectedPublicKey any,
) error {
	if leaf == nil || len(leaf.URIs) != 1 {
		return fmt.Errorf("worker certificate requires exactly one URI SAN")
	}
	actualIdentity, err := ParseIdentityURI(leaf.URIs[0])
	if err != nil || actualIdentity != identity {
		return fmt.Errorf("worker certificate URI SAN does not match the requested identity")
	}
	if expectedPublicKey != nil &&
		!publicKeysEqual(leaf.PublicKey, expectedPublicKey) {
		return fmt.Errorf("worker certificate public key does not match its CSR")
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		return fmt.Errorf("worker certificate is missing client-auth extended key usage")
	}
	return nil
}

func parseCertificateAuthority(
	certPEM, keyPEM []byte,
) (*x509.Certificate, crypto.Signer, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("decode worker CA certificate")
	}
	certificate, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse worker CA certificate: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("decode worker CA private key")
	}
	key, err := parsePrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !certificate.IsCA ||
		certificate.KeyUsage&x509.KeyUsageCertSign == 0 ||
		!publicKeysEqual(certificate.PublicKey, key.Public()) {
		return nil, nil, fmt.Errorf(
			"worker CA certificate/key mismatch or certificate is not a CA",
		)
	}
	return certificate, key, nil
}

func validateIssuerWindow(
	certificate *x509.Certificate,
	now time.Time,
	ttl time.Duration,
) error {
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return fmt.Errorf("worker CA certificate is not currently valid")
	}
	if now.Add(ttl).After(certificate.NotAfter) {
		return fmt.Errorf("worker certificate TTL exceeds remaining issuer validity")
	}
	return nil
}

func certificateFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func parsePrivateKey(der []byte) (crypto.Signer, error) {
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if signer, ok := key.(crypto.Signer); ok {
			return signer, nil
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(der); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("parse worker CA private key")
}

func publicKeysEqual(left, right any) bool {
	l, err := x509.MarshalPKIXPublicKey(left)
	if err != nil {
		return false
	}
	r, err := x509.MarshalPKIXPublicKey(right)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(l, r) == 1
}
