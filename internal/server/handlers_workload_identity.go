package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/iam"
)

func (s *Server) handleWorkloadIdentityInventory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.jobLedger == nil {
		writeIAMError(w, http.StatusServiceUnavailable, "workload identity authority is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	issuers, certificates, err := s.jobLedger.WorkloadIdentityInventory(ctx)
	cancel()
	if err != nil {
		writeIAMError(w, http.StatusServiceUnavailable, "workload identity inventory is unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuers": issuers, "certificates": certificates,
		"certificate_limit": 100,
	})
}

type workloadRevokeRequest struct {
	Fingerprint string `json:"fingerprint"`
	Reason      string `json:"reason"`
}

func decodeWorkloadRevokeRequest(
	w http.ResponseWriter,
	r *http.Request,
) (workloadRevokeRequest, bool) {
	var request workloadRevokeRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeIAMError(w, http.StatusBadRequest, "invalid workload revoke request")
		return request, false
	}
	request.Fingerprint = strings.ToLower(strings.TrimSpace(request.Fingerprint))
	request.Reason = strings.TrimSpace(request.Reason)
	if len(request.Fingerprint) != 64 || request.Reason == "" ||
		len(request.Reason) > 4096 {
		writeIAMError(w, http.StatusBadRequest, "fingerprint and revoke reason are required")
		return request, false
	}
	return request, true
}

func (s *Server) handleWorkloadCertificateRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.jobLedger == nil {
		writeIAMError(w, http.StatusServiceUnavailable, "workload identity authority is unavailable")
		return
	}
	request, ok := decodeWorkloadRevokeRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	err := s.jobLedger.RevokeWorkloadCertificate(
		ctx, request.Fingerprint, request.Reason,
	)
	cancel()
	principal, _ := iam.PrincipalFromContext(r.Context())
	if err != nil {
		s.auditRequest(r, principal, "workload.certificate.revoke",
			"workload_certificate", request.Fingerprint, "", "failure",
			map[string]any{"reason": request.Reason})
		writeIAMError(w, http.StatusNotFound, err.Error())
		return
	}
	s.auditRequest(r, principal, "workload.certificate.revoke",
		"workload_certificate", request.Fingerprint, "", "success",
		map[string]any{"reason": request.Reason})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"fingerprint": request.Fingerprint, "state": "revoked",
	})
}

func (s *Server) handleWorkloadIssuerRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.jobLedger == nil {
		writeIAMError(w, http.StatusServiceUnavailable, "workload identity authority is unavailable")
		return
	}
	request, ok := decodeWorkloadRevokeRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	err := s.jobLedger.RevokeWorkloadIssuer(
		ctx, request.Fingerprint, request.Reason,
	)
	cancel()
	principal, _ := iam.PrincipalFromContext(r.Context())
	if err != nil {
		s.auditRequest(r, principal, "workload.issuer.revoke",
			"workload_issuer", request.Fingerprint, "", "failure",
			map[string]any{"reason": request.Reason})
		writeIAMError(w, http.StatusNotFound, err.Error())
		return
	}
	s.auditRequest(r, principal, "workload.issuer.revoke",
		"workload_issuer", request.Fingerprint, "", "success",
		map[string]any{"reason": request.Reason})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"fingerprint": request.Fingerprint, "state": "revoked",
	})
}
