package builder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/workergateway"
	"github.com/slchris/portage-engine/pkg/config"
)

const (
	pullBuildLogLimit = 2 << 20

	// pullDialTimeout and pullTLSHandshakeTimeout replace what a hand-built
	// http.Transport silently gives up: none of http.DefaultTransport's bounds
	// are inherited. Without them a blackholed dial or a stalled TLS handshake
	// parks client.Do on the only context this agent has — the process-lifetime
	// one — and the single sequential pull loop stops claiming work while the
	// worker still looks alive.
	pullDialTimeout         = 30 * time.Second
	pullTLSHandshakeTimeout = 10 * time.Second

	// pullUploadLease mirrors the worker gateway broker's upload-slot lease.
	// ResponseHeaderTimeout cannot bound an artifact PUT — it starts only after
	// the body has finished writing, and the HTTP/2 transport ignores it — so
	// the collect task carries its own deadline. Past the lease the broker can
	// hand the slot to a new claim, which is exactly the point where finishing
	// the stream can no longer commit anything.
	pullUploadLease = 30 * time.Minute
)

// RunPullAgent runs the worker's only network control surface. It long-polls a
// dedicated mTLS gateway and never binds an inbound build port.
func RunPullAgent(ctx context.Context, cfg *config.BuilderConfig, local *LocalBuilder) error {
	baseURL, clients, err := pullHTTPClients(cfg)
	if err != nil {
		return err
	}
	backoff := time.Second
	lastPullError := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		task, err := pullOne(ctx, clients.control, baseURL)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if message := err.Error(); message != lastPullError {
				log.Printf("Worker gateway pull unavailable; retrying: %v", err)
				lastPullError = message
			}
			if !waitPullRetry(ctx, backoff) {
				return nil
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		if lastPullError != "" {
			log.Printf("Worker gateway pull connection restored")
			lastPullError = ""
		}
		backoff = time.Second
		if task == nil {
			continue
		}
		completion := executePullTask(ctx, clients, baseURL, local, *task)
		if err := postCompletionWithRetry(ctx, clients.control, baseURL, completion); err != nil {
			// The broker can accept the completion immediately before shutdown
			// cancels the HTTP request. The scheduler already has the result in
			// that case, so cancellation is a clean agent stop rather than a
			// worker failure. All errors while the agent is still live remain
			// fatal and fence the attempt.
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("post task completion: %w", err)
		}
	}
}

func waitPullRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func postCompletionWithRetry(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	completion workergateway.Completion,
) error {
	backoff := time.Second
	lastError := ""
	for {
		retry, err := postCompletion(ctx, client, baseURL, completion)
		if err == nil {
			if lastError != "" {
				log.Printf("Worker gateway completion delivery restored")
			}
			return nil
		}
		if !retry || ctx.Err() != nil {
			return err
		}
		if message := err.Error(); message != lastError {
			log.Printf("Worker gateway completion unavailable; retrying: %v", err)
			lastError = message
		}
		if !waitPullRetry(ctx, backoff) {
			return ctx.Err()
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// pullClients separates the two very different network shapes that share this
// one mTLS origin. Control calls are small and must fail fast; an artifact PUT
// streams a whole binary package under the broker's upload lease. Go's
// Client.Timeout also covers the request body write, so a single deadline
// cannot serve both: a several-hundred-MB gpkg on anything but a fast link
// would abort mid-body, the broker would delete the temporary generation, and
// the collect phase would destroy the VM with no way to recover.
type pullClients struct {
	control  *http.Client
	transfer *http.Client
}

func pullHTTPClients(cfg *config.BuilderConfig) (string, *pullClients, error) {
	parsed, err := url.Parse(strings.TrimSpace(cfg.WorkerGatewayURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return "", nil, fmt.Errorf("WORKER_GATEWAY_URL must be an HTTPS origin without credentials, path, query, or fragment")
	}
	certificate, err := tls.LoadX509KeyPair(cfg.WorkerTLSCert, cfg.WorkerTLSKey)
	if err != nil {
		return "", nil, fmt.Errorf("load worker TLS identity: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.WorkerTLSCA)
	if err != nil {
		return "", nil, fmt.Errorf("read worker gateway CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return "", nil, fmt.Errorf("parse worker gateway CA")
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    roots, Certificates: []tls.Certificate{certificate},
	}
	control := pullTransport(tlsConfig)
	// The transfer client is bounded at the transport instead: a stalled
	// connection still fails, but a slow one is allowed to run to the upload
	// lease the collect task's context carries. The response header budget
	// starts only after the body is written, and the broker hashes and commits
	// multi-GB uploads before answering.
	transfer := pullTransport(tlsConfig.Clone())
	transfer.ResponseHeaderTimeout = 5 * time.Minute
	transfer.IdleConnTimeout = 90 * time.Second
	return strings.TrimRight(parsed.String(), "/"), &pullClients{
		control:  &http.Client{Transport: control, Timeout: 40 * time.Second},
		transfer: &http.Client{Transport: transfer},
	}, nil
}

// pullTransport carries the connection-establishment bounds both clients need
// regardless of how their request bodies differ.
func pullTransport(tlsConfig *tls.Config) *http.Transport {
	return &http.Transport{
		TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true,
		DialContext: (&net.Dialer{
			Timeout: pullDialTimeout, KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: pullTLSHandshakeTimeout,
	}
}

// pullTaskContext bounds one claimed task. Only the collect action makes a
// network call the agent cannot otherwise abandon, and the broker's upload
// lease is the deadline that already governs it. Build and verify run entirely
// against the local builder and legitimately take hours, so they keep the
// agent's own lifetime as their only bound.
func pullTaskContext(
	ctx context.Context,
	action string,
) (context.Context, context.CancelFunc) {
	if action == workergateway.ActionCollect {
		return context.WithTimeout(ctx, pullUploadLease)
	}
	return context.WithCancel(ctx)
}

func pullOne(ctx context.Context, client *http.Client, baseURL string) (*workergateway.Task, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/pull", nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("worker gateway pull returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var task workergateway.Task
	decoder := json.NewDecoder(io.LimitReader(response.Body, MaxBuildRequestBodyBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&task); err != nil {
		return nil, fmt.Errorf("decode worker task: %w", err)
	}
	if task.ID == "" {
		return nil, fmt.Errorf("worker task has no id")
	}
	return &task, nil
}

func executePullTask(
	ctx context.Context,
	clients *pullClients,
	baseURL string,
	local *LocalBuilder,
	task workergateway.Task,
) workergateway.Completion {
	taskCtx, cancel := pullTaskContext(ctx, task.Action)
	defer cancel()
	payload, err := executePullAction(taskCtx, clients, baseURL, local, task)
	completion := workergateway.Completion{
		TaskID: task.ID, DeliveryFence: task.DeliveryFence,
	}
	if err != nil {
		completion.Error = err.Error()
		return completion
	}
	completion.Payload = payload
	return completion
}

func executePullAction(
	ctx context.Context,
	clients *pullClients,
	baseURL string,
	local *LocalBuilder,
	task workergateway.Task,
) (json.RawMessage, error) {
	switch task.Action {
	case workergateway.ActionBuild:
		var request LocalBuildRequest
		if err := decodePullPayload(task.Payload, &request); err != nil {
			return nil, err
		}
		request.ExecutionID = task.ID
		jobID, err := local.SubmitBuild(&request)
		if err != nil {
			return nil, err
		}
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
				job, err := local.GetJobStatus(jobID)
				if err != nil {
					return nil, err
				}
				if job.Status == "failed" || job.Status == "success" || job.Status == "completed" {
					if len(job.Log) > pullBuildLogLimit {
						job.Log = job.Log[len(job.Log)-pullBuildLogLimit:]
					}
					// Failed builds are returned as typed results rather than a
					// transport error so fenced compile telemetry is not discarded.
					return json.Marshal(job)
				}
			}
		}
	case workergateway.ActionVerify:
		var request VerifyInstallRequest
		if err := decodePullPayload(task.Payload, &request); err != nil {
			return nil, err
		}
		output, err := local.VerifyInstall(request)
		result := struct {
			OK    bool   `json:"ok"`
			Log   string `json:"log"`
			Error string `json:"error,omitempty"`
		}{OK: err == nil, Log: output}
		if err != nil {
			result.Error = err.Error()
		}
		return json.Marshal(result)
	case workergateway.ActionCollect:
		var request workergateway.CollectRequest
		if err := decodePullPayload(task.Payload, &request); err != nil {
			return nil, err
		}
		path, err := local.GetArtifactPathByRel(request.LocalJobID, request.Relative)
		if err != nil {
			return nil, err
		}
		return uploadPullArtifact(ctx, clients.transfer, baseURL, request.UploadID, path)
	default:
		return nil, fmt.Errorf("unsupported worker action %q", task.Action)
	}
}

func decodePullPayload(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode task payload: %w", err)
	}
	return nil
}

func uploadPullArtifact(
	ctx context.Context,
	client *http.Client,
	baseURL, uploadID, path string,
) (json.RawMessage, error) {
	file, err := os.Open(path) // #nosec G304 -- path was resolved against the builder's recorded artifact list.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut,
		baseURL+"/v1/uploads/"+url.PathEscape(uploadID), file)
	if err != nil {
		return nil, err
	}
	request.ContentLength = info.Size()
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Content-SHA256", hex.EncodeToString(hash.Sum(nil)))
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("artifact upload returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var result workergateway.CollectResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func postCompletion(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	completion workergateway.Completion,
) (bool, error) {
	body, err := json.Marshal(completion)
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/complete", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return true, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		retry := response.StatusCode == http.StatusRequestTimeout ||
			response.StatusCode == http.StatusTooManyRequests ||
			response.StatusCode >= http.StatusInternalServerError
		return retry, fmt.Errorf("worker completion returned %d: %s",
			response.StatusCode, strings.TrimSpace(string(data)))
	}
	return false, nil
}
