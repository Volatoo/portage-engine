package workergateway

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNoWork is returned when a worker has no command ready for delivery.
	ErrNoWork = errors.New("no worker command is ready")
	// ErrResultPending means a durable command exists but is not terminal.
	ErrResultPending = errors.New("worker command result is pending")
	// ErrStaleFence means another delivery or upload generation superseded the caller.
	ErrStaleFence = errors.New("worker gateway fence is stale")
)

// UploadClaim is a fenced filesystem upload capability. Each fence writes a
// different temporary generation; only the database-authorized generation may
// be atomically promoted to Destination.
type UploadClaim struct {
	ID          string
	Destination string
	MaxBytes    int64
	Fence       int64
	Completed   bool
	Digest      string
	Size        int64
}

// DurableStore is the PostgreSQL authority used by a multi-replica Broker.
// Its method names are gateway-specific to avoid accidentally treating the
// general scheduler lease as a command-delivery lease.
type DurableStore interface {
	RegisterWorkerSession(context.Context, Identity) error
	RegisterWorkerCertificate(context.Context, Identity, CertificateRecord) error
	AuthorizeWorkerCertificate(context.Context, Identity, PresentedCertificate) error
	RevokeWorkerSession(context.Context, Identity, string) error
	TouchWorkerSession(context.Context, Identity) error
	WorkerSessionConnected(context.Context, Identity) (bool, error)

	EnqueueWorkerCommand(context.Context, Identity, Task) error
	ClaimWorkerCommand(context.Context, Identity, time.Duration) (*Task, error)
	CompleteWorkerCommand(context.Context, Identity, Completion) error
	WorkerCommandResult(context.Context, Identity, string) (*Completion, error)

	PrepareWorkerUpload(context.Context, Identity, string, string, int64) error
	ClaimWorkerUpload(context.Context, Identity, string, time.Duration) (UploadClaim, error)
	CompleteWorkerUpload(context.Context, Identity, string, int64, string, int64) error
	CancelWorkerUpload(context.Context, Identity, string, int64) error

	WorkerGatewayStatus(context.Context) (RuntimeStatus, error)
}
