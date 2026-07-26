// Package builder provides local and remote build capabilities.
package builder

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/slchris/portage-engine/internal/notification"
	"github.com/slchris/portage-engine/internal/signing"
	"github.com/slchris/portage-engine/pkg/config"
)

// LocalBuildRequest represents a local package build request.
type LocalBuildRequest struct {
	PackageName   string            `json:"package_name"`
	Version       string            `json:"version"`
	Arch          string            `json:"arch,omitempty"`
	ProfileID     string            `json:"profile_id,omitempty"`
	RepositoryIDs []string          `json:"repository_ids,omitempty"`
	ResourceClass string            `json:"resource_class,omitempty"`
	UseFlags      map[string]string `json:"use_flags"`
	Environment   map[string]string `json:"environment"`
	ConfigBundle  *ConfigBundle     `json:"config_bundle,omitempty"`
	PackageSpecs  []PackageSpec     `json:"package_specs,omitempty"`
}

// BuildJob represents a build job with its status.
//
// The mutable fields (Status, Log, ArtifactURL, Error, EndTime, Metadata) are
// written by the worker/executor goroutine while HTTP handler goroutines read
// them, so all access must go through the mu-guarded helpers below.
type BuildJob struct {
	mu          sync.Mutex         `json:"-"`
	ID          string             `json:"id"`
	Request     *LocalBuildRequest `json:"request"`
	Status      string             `json:"status"` // queued, building, success, failed
	StartTime   time.Time          `json:"start_time"`
	EndTime     time.Time          `json:"end_time"`
	Log         string             `json:"log"`
	ArtifactURL string             `json:"artifact_url"`
	// Artifacts lists every binary package produced by the build, as paths
	// relative to the artifact dir with the category preserved
	// (e.g. "app-misc/jq-1.8.1-1.gpkg.tar").
	Artifacts []string               `json:"artifacts,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// appendLog appends to the job log under the job lock.
func (j *BuildJob) appendLog(s string) {
	j.mu.Lock()
	j.Log += s
	j.mu.Unlock()
}

// setArtifacts records the full produced-artifact list under the job lock.
func (j *BuildJob) setArtifacts(rels []string) {
	j.mu.Lock()
	j.Artifacts = rels
	j.mu.Unlock()
}

// artifactsSnapshot returns a copy of the produced-artifact list.
func (j *BuildJob) artifactsSnapshot() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.Artifacts...)
}

// setArtifactURL sets the artifact URL under the job lock.
func (j *BuildJob) setArtifactURL(url string) {
	j.mu.Lock()
	j.ArtifactURL = url
	j.mu.Unlock()
}

// snapshot returns a copy of the status and artifact URL under the job lock.
func (j *BuildJob) snapshot() (status, artifactURL string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Status, j.ArtifactURL
}

// Clone returns a deep copy of the job (without the mutex) taken under the job
// lock. Use this instead of copying a BuildJob by value, which would copy the
// mutex.
func (j *BuildJob) Clone() *BuildJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	c := &BuildJob{
		ID:          j.ID,
		Request:     j.Request,
		Status:      j.Status,
		StartTime:   j.StartTime,
		EndTime:     j.EndTime,
		Log:         j.Log,
		ArtifactURL: j.ArtifactURL,
		Artifacts:   append([]string(nil), j.Artifacts...),
		Error:       j.Error,
	}
	if j.Metadata != nil {
		c.Metadata = make(map[string]interface{}, len(j.Metadata))
		for k, v := range j.Metadata {
			c.Metadata[k] = v
		}
	}
	return c
}

// LocalBuilder handles build jobs in a native Gentoo disposable root.
type LocalBuilder struct {
	workers          int
	jobQueue         chan *BuildJob
	jobs             map[string]*BuildJob
	jobsMutex        sync.RWMutex
	storageUpload    *StorageUploader
	workDir          string
	artifactDir      string
	executor         *BuildExecutor
	notifier         *notification.Notifier
	jobStore         *JobStore
	persister        *JobPersister
	instanceID       string
	architecture     string
	pkgMgr           PackageManager
	cfg              *config.BuilderConfig
	nativeStateMu    sync.Mutex
	nativeJobPolicy  string
	nativeConsumed   bool
	nativeMarkerPath string
	nativeJobID      string
}

// ErrNativeBuilderDraining means a single-use native builder has already
// accepted its lifetime BuildJob. The host root may now contain arbitrary VDB,
// filesystem and ebuild post-install state, so only an external snapshot/VM
// reset can make it eligible again.
var ErrNativeBuilderDraining = errors.New("native builder is single-use and already consumed; reset the VM/rootfs snapshot before accepting another job")

type nativeTaintRecord struct {
	JobID      string    `json:"job_id"`
	ReservedAt time.Time `json:"reserved_at"`
	Reason     string    `json:"reason"`
}

// NewLocalBuilder creates a new local builder instance.
func NewLocalBuilder(workers int, cfg *config.BuilderConfig) *LocalBuilder {
	return newLocalBuilderWithConfig(workers, cfg)
}

// newLocalBuilderWithConfig creates a new local builder with the given configuration.
func newLocalBuilderWithConfig(workers int, cfg *config.BuilderConfig) *LocalBuilder {
	workDir := getWorkDir(cfg)
	artifactDir := getArtifactDir(cfg)
	ensureDirectories(workDir, artifactDir)
	notifier := loadNotifier(cfg)
	storageUpload := initStorageUploader(cfg)
	executor := initBuildExecutor(cfg)
	pkgMgr := initPackageManager(cfg)
	jobStore := initJobStore(cfg)

	instanceID := generateInstanceID(cfg)
	architecture := getArchitecture(cfg)
	nativeJobPolicy, nativeMarkerPath, nativeConsumed, nativeJobID := nativeReuseState(cfg)

	lb := &LocalBuilder{
		workers:          workers,
		jobQueue:         make(chan *BuildJob, 100),
		jobs:             make(map[string]*BuildJob),
		storageUpload:    storageUpload,
		workDir:          workDir,
		artifactDir:      artifactDir,
		executor:         executor,
		notifier:         notifier,
		jobStore:         jobStore,
		instanceID:       instanceID,
		architecture:     architecture,
		pkgMgr:           pkgMgr,
		cfg:              cfg,
		nativeJobPolicy:  nativeJobPolicy,
		nativeConsumed:   nativeConsumed,
		nativeMarkerPath: nativeMarkerPath,
		nativeJobID:      nativeJobID,
	}

	if jobStore != nil {
		loadedJobs, err := jobStore.Load()
		if err != nil {
			log.Printf("Failed to load persisted jobs: %v", err)
		} else {
			lb.jobs = loadedJobs
			log.Printf("Loaded %d persisted jobs", len(loadedJobs))
			reconcileLoadedJobs(jobStore, loadedJobs)
		}

		// Construct the persister so job state actually survives restarts.
		// Without this, saveJobState()/Stop() were no-ops (lb.persister was nil).
		retention := time.Duration(cfg.RetentionDays) * 24 * time.Hour
		lb.persister = NewJobPersister(jobStore, lb.jobsSnapshot, 30*time.Second, retention)
		lb.persister.Start()
	}

	for i := 0; i < workers; i++ {
		go lb.worker(i)
	}

	return lb
}

func nativeReuseState(cfg *config.BuilderConfig) (policy, markerPath string, consumed bool, jobID string) {
	// Native-only builders fail safe even when constructed without a config.
	policy = "single-use"
	if cfg != nil {
		switch cfg.NativeJobPolicy {
		case "", "single-use":
			policy = "single-use"
		case "unsafe-reuse":
			policy = "unsafe-reuse"
		default:
			policy = "single-use"
		}
	}
	if policy != "single-use" {
		return policy, "", false, ""
	}

	dataDir := "/var/lib/portage-engine"
	if cfg != nil && cfg.DataDir != "" {
		dataDir = cfg.DataDir
	}
	markerPath = filepath.Join(dataDir, "native-builder-tainted.json")
	data, err := os.ReadFile(markerPath)
	if os.IsNotExist(err) {
		return policy, markerPath, false, ""
	}
	if err != nil {
		// An unreadable marker is still evidence that a prior native job may
		// have consumed this root. Never turn a permissions/disk fault into a
		// clean-builder decision.
		return policy, markerPath, true, ""
	}
	var record nativeTaintRecord
	if json.Unmarshal(data, &record) == nil {
		jobID = record.JobID
	}
	return policy, markerPath, true, jobID
}

// reserveNativeLifetime atomically consumes a single-use native builder before
// the job is queued. The durable marker makes a service restart fail closed:
// restarting only the process cannot clean a host root mutated by emerge.
func (lb *LocalBuilder) reserveNativeLifetime(jobID string) error {
	if lb.nativeJobPolicy != "single-use" {
		return nil
	}
	lb.nativeStateMu.Lock()
	defer lb.nativeStateMu.Unlock()
	if lb.nativeConsumed {
		return ErrNativeBuilderDraining
	}
	if err := os.MkdirAll(filepath.Dir(lb.nativeMarkerPath), 0o750); err != nil {
		return fmt.Errorf("create native taint state directory: %w", err)
	}
	record, err := json.Marshal(nativeTaintRecord{
		JobID:      jobID,
		ReservedAt: time.Now().UTC(),
		Reason:     "native emerge can mutate the host root; external reset required",
	})
	if err != nil {
		return err
	}
	marker, err := os.OpenFile(lb.nativeMarkerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			lb.nativeConsumed = true
			return ErrNativeBuilderDraining
		}
		return fmt.Errorf("create native taint marker: %w", err)
	}
	// From this point on, fail closed even if writing or syncing the marker
	// fails. The exclusive marker exists, and a storage fault must not make
	// this process advertise the root as reusable.
	lb.nativeConsumed = true
	lb.nativeJobID = jobID
	if _, err := marker.Write(record); err != nil {
		_ = marker.Close()
		return err
	}
	if err := marker.Sync(); err != nil {
		_ = marker.Close()
		return err
	}
	if err := marker.Close(); err != nil {
		return err
	}
	return nil
}

// releaseNativeLifetime is used only when queue admission fails before any
// worker can execute the job. Once admitted, the marker is intentionally never
// cleared by the builder process.
func (lb *LocalBuilder) releaseNativeLifetime(jobID string) {
	if lb.nativeJobPolicy != "single-use" {
		return
	}
	lb.nativeStateMu.Lock()
	defer lb.nativeStateMu.Unlock()
	if !lb.nativeConsumed || lb.nativeJobID != jobID {
		return
	}
	if err := os.Remove(lb.nativeMarkerPath); err != nil && !os.IsNotExist(err) {
		log.Printf("Warning: failed to roll back unused native taint marker: %v", err)
		return
	}
	lb.nativeConsumed = false
	lb.nativeJobID = ""
}

// AcceptingBuilds reports whether this service may accept another BuildJob.
// Artifact/status/verification endpoints remain available while draining.
func (lb *LocalBuilder) AcceptingBuilds() bool {
	lb.nativeStateMu.Lock()
	defer lb.nativeStateMu.Unlock()
	return lb.nativeJobPolicy != "single-use" || !lb.nativeConsumed
}

// Capacity is the currently safe admission capacity. A fresh native single-use
// builder can accept one lifetime job even if legacy configuration requested
// more worker goroutines; once consumed it advertises zero.
func (lb *LocalBuilder) Capacity() int {
	if lb.nativeJobPolicy == "single-use" {
		lb.nativeStateMu.Lock()
		defer lb.nativeStateMu.Unlock()
		if lb.nativeConsumed {
			return 0
		}
		return 1
	}
	return lb.workers
}

// getWorkDir returns the work directory from config or environment.
func getWorkDir(cfg *config.BuilderConfig) string {
	workDir := os.Getenv("BUILD_WORK_DIR")
	if workDir == "" {
		if cfg != nil && cfg.WorkDir != "" {
			return cfg.WorkDir
		}
		return "/var/tmp/portage-builds"
	}
	return workDir
}

// getArtifactDir returns the artifact directory from config or environment.
func getArtifactDir(cfg *config.BuilderConfig) string {
	artifactDir := os.Getenv("BUILD_ARTIFACT_DIR")
	if artifactDir == "" {
		if cfg != nil && cfg.ArtifactDir != "" {
			return cfg.ArtifactDir
		}
		return "/var/tmp/portage-artifacts"
	}
	return artifactDir
}

// ensureDirectories creates and verifies the work and artifact directories.
func ensureDirectories(workDir, artifactDir string) {
	_ = os.MkdirAll(workDir, 0750)
	_ = os.MkdirAll(artifactDir, 0750)
	verifyDirectoryWritable(workDir, "Work")
	verifyDirectoryWritable(artifactDir, "Artifact")
}

// verifyDirectoryWritable checks if a directory is writable.
func verifyDirectoryWritable(dir, dirType string) {
	testFile := filepath.Join(dir, ".write_test")
	if err := os.WriteFile(testFile, []byte("test"), 0600); err != nil {
		log.Printf("WARNING: %s directory %s is not writable: %v", dirType, dir, err)
		log.Printf("Please ensure the directory exists and is owned by the service user")
	} else {
		_ = os.Remove(testFile)
	}
}

// loadNotifier loads the notification configuration.
func loadNotifier(cfg *config.BuilderConfig) *notification.Notifier {
	notifyConfigPath := os.Getenv("NOTIFY_CONFIG")
	if notifyConfigPath == "" {
		if cfg != nil && cfg.NotifyConfig != "" {
			notifyConfigPath = cfg.NotifyConfig
		} else {
			notifyConfigPath = "configs/notification.json"
		}
	}

	notifyConfig, err := notification.LoadConfig(notifyConfigPath)
	if err == nil {
		log.Printf("Notification system loaded from %s", notifyConfigPath)
		return notification.NewNotifier(notifyConfig)
	}
	log.Printf("Notification config not loaded (optional): %v", err)
	return nil
}

// initStorageUploader initializes the storage uploader if configured.
func initStorageUploader(cfg *config.BuilderConfig) *StorageUploader {
	if cfg == nil {
		return nil
	}

	storageType := cfg.StorageType
	if storageType == "" {
		storageType = "local"
	}

	uploader, err := NewStorageUploader(
		storageType,
		cfg.StorageLocalDir,
		cfg.StorageS3Bucket,
		cfg.StorageS3Region,
		cfg.StorageS3Prefix,
		cfg.StorageHTTPBase,
	)
	if err != nil {
		log.Printf("Failed to initialize storage uploader: %v", err)
		return nil
	}

	log.Printf("Storage uploader initialized with type: %s, enabled: %v", storageType, uploader.IsEnabled())
	return uploader
}

// buildOptionsFromConfig derives the executor's unsigned output format. Signing
// is deliberately absent from builders: only the isolated signer may hold the
// private key or mutate a verified artifact generation.
func buildOptionsFromConfig(cfg *config.BuilderConfig) BuildOptions {
	format := "gpkg"
	if cfg != nil && cfg.BinpkgFormat != "" {
		format = cfg.BinpkgFormat
	}
	return BuildOptions{Format: format}
}

// initBuildExecutor initializes the build executor.
func initBuildExecutor(cfg *config.BuilderConfig) *BuildExecutor {
	workDir := getWorkDir(cfg)
	artifactDir := getArtifactDir(cfg)
	return NewBuildExecutorWithOptions(workDir, artifactDir, buildOptionsFromConfig(cfg))
}

// initPackageManager initializes the package manager.
func initPackageManager(cfg *config.BuilderConfig) PackageManager {
	if cfg == nil {
		cfg = &config.BuilderConfig{
			PortageReposPath: "/var/db/repos",
			PortageConfPath:  "/etc/portage",
			MakeConfPath:     "/etc/portage/make.conf",
		}
	}
	return NewPackageManager(cfg)
}

// initJobStore initializes the job store and loads persisted jobs.
func initJobStore(cfg *config.BuilderConfig) *JobStore {
	if cfg == nil || !cfg.PersistenceEnabled {
		return nil
	}

	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "/var/lib/portage-engine"
	}

	jobStore, err := NewJobStore(dataDir)
	if err != nil {
		log.Printf("Failed to initialize job store: %v (persistence disabled)", err)
		return nil
	}

	return jobStore
}

// reconcileLoadedJobs marks any persisted jobs that were left in the
// "building" state (e.g. due to a crash) as "failed", since the build
// process did not survive the restart. The reconciled state is persisted.
func reconcileLoadedJobs(jobStore *JobStore, jobs map[string]*BuildJob) {
	reconciled := 0
	for _, job := range jobs {
		if job.Status == "building" || job.Status == "queued" {
			job.Status = "failed"
			if job.Error == "" {
				job.Error = "build interrupted by builder restart"
			}
			if job.EndTime.IsZero() {
				job.EndTime = time.Now()
			}
			reconciled++
		}
	}

	if reconciled == 0 {
		return
	}

	log.Printf("Reconciled %d interrupted job(s) to failed on startup", reconciled)
	if err := jobStore.Save(jobs); err != nil {
		log.Printf("Failed to persist reconciled jobs: %v", err)
	}
}

// generateInstanceID generates or retrieves the instance ID.
func generateInstanceID(cfg *config.BuilderConfig) string {
	if cfg != nil && cfg.InstanceID != "" {
		return cfg.InstanceID
	}

	if hostname, err := os.Hostname(); err == nil {
		return hostname
	}

	return uuid.New().String()[:8]
}

// getArchitecture detects or retrieves the system architecture.
func getArchitecture(cfg *config.BuilderConfig) string {
	if cfg != nil && cfg.Architecture != "" {
		return cfg.Architecture
	}
	return detectArchitecture()
}

// SubmitBuild submits a new build job.
func (lb *LocalBuilder) SubmitBuild(req *LocalBuildRequest) (string, error) {
	// Validate every untrusted field before the request can reach any build
	// native emerge argv. This is the single choke point that closes emerge
	// option injection.
	if err := validateLocalBuildRequest(req); err != nil {
		return "", fmt.Errorf("invalid build request: %w", err)
	}

	jobID := uuid.New().String()
	if err := lb.reserveNativeLifetime(jobID); err != nil {
		return "", err
	}

	job := &BuildJob{
		ID:        jobID,
		Request:   req,
		Status:    "queued",
		StartTime: time.Now(),
		Metadata:  make(map[string]interface{}),
	}

	lb.jobsMutex.Lock()
	lb.jobs[jobID] = job
	lb.jobsMutex.Unlock()

	// Non-blocking send: if the queue is full, reject the job instead of
	// blocking the calling (HTTP handler) goroutine indefinitely.
	select {
	case lb.jobQueue <- job:
		return jobID, nil
	default:
		lb.jobsMutex.Lock()
		delete(lb.jobs, jobID)
		lb.jobsMutex.Unlock()
		lb.releaseNativeLifetime(jobID)
		return "", fmt.Errorf("builder queue full")
	}
}

// GetJobStatus returns the status of a build job.
func (lb *LocalBuilder) GetJobStatus(jobID string) (*BuildJob, error) {
	lb.jobsMutex.RLock()
	defer lb.jobsMutex.RUnlock()

	job, exists := lb.jobs[jobID]
	if !exists {
		return nil, fmt.Errorf("job not found: %s", jobID)
	}

	// Return a clone so callers (e.g. JSON-encoding HTTP handlers) never read
	// the live job's mutable fields while the worker is writing them.
	return job.Clone(), nil
}

// ListJobs returns all build jobs.
func (lb *LocalBuilder) ListJobs() []*BuildJob {
	lb.jobsMutex.RLock()
	defer lb.jobsMutex.RUnlock()

	jobs := make([]*BuildJob, 0, len(lb.jobs))
	for _, job := range lb.jobs {
		// Clone so concurrent worker writes don't race the caller's reads.
		jobs = append(jobs, job.Clone())
	}

	return jobs
}

// jobsSnapshot returns a deep copy of the jobs map for persistence. Clone()
// copies each job's fields under its lock (without the mutex), so this is safe
// to call while workers are mutating jobs.
func (lb *LocalBuilder) jobsSnapshot() map[string]*BuildJob {
	lb.jobsMutex.RLock()
	defer lb.jobsMutex.RUnlock()
	out := make(map[string]*BuildJob, len(lb.jobs))
	for k, v := range lb.jobs {
		out[k] = v.Clone()
	}
	return out
}

// Shutdown gracefully shuts down the builder and persists jobs.
func (lb *LocalBuilder) Shutdown() {
	if lb.persister != nil {
		lb.persister.Stop()
	}
}

// ActiveJobs returns the number of jobs currently queued or building, for
// heartbeat/capacity reporting to the central server.
func (lb *LocalBuilder) ActiveJobs() int {
	lb.jobsMutex.RLock()
	defer lb.jobsMutex.RUnlock()

	n := 0
	for _, job := range lb.jobs {
		if job.Status == "queued" || job.Status == "building" {
			n++
		}
	}
	return n
}

// GetStatus returns a snapshot of the builder's job counts and mode.
func (lb *LocalBuilder) GetStatus() map[string]interface{} {
	lb.jobsMutex.RLock()
	defer lb.jobsMutex.RUnlock()

	queued := 0
	building := 0
	completed := 0
	failed := 0

	for _, job := range lb.jobs {
		switch job.Status {
		case "queued":
			queued++
		case "building":
			building++
		case "success":
			completed++
		case "failed":
			failed++
		}
	}

	// Get system resource information
	sysInfo := GetSystemInfo()

	// Determine status based on current load
	status := "online"
	if building >= lb.workers {
		status = "busy"
	}
	if !lb.AcceptingBuilds() {
		status = "draining"
	}

	return map[string]interface{}{
		"instance_id":       lb.instanceID,
		"architecture":      lb.architecture,
		"status":            status,
		"workers":           lb.workers,
		"capacity":          lb.Capacity(),
		"current_load":      building,
		"queued":            queued,
		"building":          building,
		"completed":         completed,
		"failed":            failed,
		"total":             len(lb.jobs),
		"success_builds":    completed,
		"failed_builds":     failed,
		"total_builds":      completed + failed,
		"execution_backend": "native-gentoo",
		"native_job_policy": lb.nativeJobPolicy,
		"accepting_builds":  lb.AcceptingBuilds(),
		"cpu_usage":         sysInfo.CPUUsage,
		"memory_usage":      sysInfo.MemoryUsage,
		"disk_usage":        sysInfo.DiskUsage,
		"cpu_count":         sysInfo.CPUCount,
		"memory_total":      sysInfo.MemoryTotal,
		"memory_used":       sysInfo.MemoryUsed,
		"disk_total":        sysInfo.DiskTotal,
		"disk_used":         sysInfo.DiskUsed,
		"enabled":           true,
	}
}

// worker processes build jobs from the queue.
func (lb *LocalBuilder) worker(id int) {
	log.Printf("Worker %d started", id)

	for job := range lb.jobQueue {
		log.Printf("Worker %d processing job %s", id, job.ID)

		job.mu.Lock()
		job.Status = "building"
		job.mu.Unlock()

		// Persist the "building" transition so a crash mid-build can be
		// reconciled on the next startup instead of leaving a stuck job.
		lb.saveJobState()

		var err error
		// Check if this is a new-style config bundle build
		if job.Request.ConfigBundle != nil {
			err = lb.executeConfigBundleBuild(job)
		} else {
			// Legacy request shape, executed by the same native backend.
			err = lb.executeNativeBuild(job)
		}

		job.mu.Lock()
		job.EndTime = time.Now()
		if err != nil {
			job.Status = "failed"
			job.Error = err.Error()
			// Append log to error for visibility in API
			if job.Log != "" {
				job.Error = fmt.Sprintf("%s\n\nBuild Log:\n%s", job.Error, job.Log)
			}
			log.Printf("Worker %d: Job %s failed: %v", id, job.ID, err)
		} else {
			job.Status = "success"
			log.Printf("Worker %d: Job %s completed successfully", id, job.ID)
		}
		job.mu.Unlock()

		// Persist job state immediately after completion
		lb.saveJobState()

		// Notify asynchronously: notification channels (SMTP/webhook/Slack/
		// Telegram) run serially with timeouts up to ~30s each, which must not
		// stall the build worker. A clone is passed so the notifier never races
		// with later writes to the live job.
		go lb.sendNotification(job.Clone())
	}
}

// saveJobState saves the current job state to persistent storage.
func (lb *LocalBuilder) saveJobState() {
	if lb.persister != nil {
		if err := lb.persister.SaveNow(); err != nil {
			log.Printf("Failed to save job state: %v", err)
		}
	}
}

// executeConfigBundleBuild executes a build using configuration bundle.
func (lb *LocalBuilder) executeConfigBundleBuild(job *BuildJob) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	bundle := job.Request.ConfigBundle

	// If no package specs provided, create from legacy request
	if bundle.Packages == nil || len(bundle.Packages.Packages) == 0 {
		bundle.Packages = &BuildPackageSpec{
			Packages: []PackageSpec{
				{
					Atom:    job.Request.PackageName,
					Version: job.Request.Version,
				},
			},
		}
		// Convert legacy UseFlags map to slice
		if len(job.Request.UseFlags) > 0 {
			var useFlags []string
			for flag, enabled := range job.Request.UseFlags {
				if enabled == "true" || enabled == "1" {
					useFlags = append(useFlags, flag)
				} else {
					useFlags = append(useFlags, "-"+flag)
				}
			}
			bundle.Packages.Packages[0].UseFlags = useFlags
		}
	}

	return lb.executor.ExecuteBuild(ctx, bundle, job)
}

// prepareJobWorkDir creates and returns the job-specific work directory.
func (lb *LocalBuilder) prepareJobWorkDir(jobID string) (string, error) {
	jobWorkDir := filepath.Join(lb.workDir, jobID)
	if err := os.MkdirAll(jobWorkDir, 0750); err != nil {
		return "", fmt.Errorf("failed to create work directory: %w", err)
	}
	return jobWorkDir, nil
}

// buildUseFlagsString constructs the USE flags string.
func buildUseFlagsString(useFlags map[string]string) string {
	var flags string
	for flag, enabled := range useFlags {
		if enabled == "true" || enabled == "1" {
			flags += flag + " "
		} else {
			flags += "-" + flag + " "
		}
	}
	return flags
}

// collectAndUploadArtifact finds, copies, signs, and uploads the artifact.
func (lb *LocalBuilder) collectAndUploadArtifact(job *BuildJob, outputDir string) error {
	// Ensure filesystem is synced
	_ = exec.Command("sync").Run()
	time.Sleep(10 * time.Second)

	rels, err := lb.waitForArtifacts(outputDir)
	if err != nil {
		return err
	}

	// Copy every produced package into the artifact dir, category preserved.
	for _, rel := range rels {
		dest := filepath.Join(lb.artifactDir, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0750); err != nil {
			return fmt.Errorf("failed to create artifact dir: %w", err)
		}
		if err := exec.Command("cp", filepath.Join(outputDir, rel), dest).Run(); err != nil {
			return fmt.Errorf("failed to copy artifact %s: %w", rel, err)
		}
	}

	primary := primaryArtifact(rels, job.Request.PackageName, func(rel string) int64 {
		if info, err := os.Stat(filepath.Join(lb.artifactDir, rel)); err == nil {
			return info.Size()
		}
		return 0
	})
	destPath := filepath.Join(lb.artifactDir, primary)

	job.setArtifactURL(destPath)
	job.setArtifacts(rels)

	// Builders are an unsigned trust domain. Reject embedded signatures rather
	// than accepting a package signed by a builder-controlled key.
	if gpkgIsSigned(destPath) {
		return errors.New("builder produced a signed GPKG; signing is restricted to the isolated signer")
	}
	lb.uploadArtifact(job, destPath)

	return nil
}

// gpkgIsSigned reports whether a .gpkg.tar carries an embedded OpenPGP
// signature (a *.sig member), i.e. it was produced with binpkg-signing.
func gpkgIsSigned(path string) bool {
	f, err := os.Open(path) // #nosec G304 -- builder's own artifact dir.
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err != nil {
			return false
		}
		if strings.HasSuffix(hdr.Name, ".sig") {
			return true
		}
	}
}

// waitForArtifacts scans the container output dir for every produced binary
// package, returning paths relative to outputDir (category preserved).
func (lb *LocalBuilder) waitForArtifacts(outputDir string) ([]string, error) {
	var rels []string
	for i := 0; i < 10; i++ {
		if i > 0 {
			_ = exec.Command("sync").Run()
			time.Sleep(2 * time.Second)
		}
		rels = rels[:0]
		_ = filepath.Walk(outputDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil || info.IsDir() {
				return nil
			}
			name := filepath.Base(path)
			if strings.HasSuffix(name, ".gpkg.tar") || strings.HasSuffix(name, ".tbz2") {
				if rel, err := filepath.Rel(outputDir, path); err == nil {
					rels = append(rels, rel)
				}
			}
			return nil
		})
		if len(rels) > 0 {
			return rels, nil
		}
		if i < 4 {
			log.Printf("No artifacts found on attempt %d, retrying...", i+1)
		}
	}
	return nil, fmt.Errorf("no artifacts found in %s", outputDir)
}

// primaryArtifact picks the artifact belonging to the requested package
// (matching "<pn>-<digit>" and, when present, the category directory);
// falls back to the largest file when nothing matches.
func primaryArtifact(rels []string, pkgName string, sizeOf func(string) int64) string {
	if len(rels) == 0 {
		return ""
	}
	category, pn := "", pkgName
	if idx := strings.LastIndex(pkgName, "/"); idx >= 0 {
		category, pn = pkgName[:idx], pkgName[idx+1:]
	}
	matches := make([]string, 0, len(rels))
	for _, rel := range rels {
		base := filepath.Base(rel)
		if !strings.HasPrefix(base, pn+"-") || len(base) <= len(pn)+1 {
			continue
		}
		if c := base[len(pn)+1]; c < '0' || c > '9' {
			continue // e.g. "jq-extras-1.0" must not match "jq"
		}
		if category != "" && strings.Contains(rel, "/") && !strings.HasPrefix(rel, category+"/") {
			continue
		}
		matches = append(matches, rel)
	}
	pool := matches
	if len(pool) == 0 {
		pool = rels
	}
	best, bestSize := pool[0], int64(-1)
	for _, rel := range pool {
		if s := sizeOf(rel); s > bestSize {
			best, bestSize = rel, s
		}
	}
	return best
}

// uploadArtifact uploads the artifact to storage if configured.
func (lb *LocalBuilder) uploadArtifact(job *BuildJob, artifactPath string) {
	if lb.storageUpload != nil && lb.storageUpload.IsEnabled() {
		artifactName := filepath.Base(artifactPath)
		remotePath := artifactName
		if err := lb.storageUpload.Upload(artifactPath, remotePath); err != nil {
			log.Printf("Warning: failed to upload artifact to storage: %v", err)
		} else {
			uploadedURL, _ := lb.storageUpload.GetURL(remotePath)
			job.setArtifactURL(uploadedURL)
			job.Metadata["uploaded"] = true
			log.Printf("Artifact uploaded to storage: %s", uploadedURL)
		}
	}
}

// executeNativeBuild performs the build natively using the system package manager.
func (lb *LocalBuilder) executeNativeBuild(job *BuildJob) error {
	jobWorkDir, err := lb.prepareJobWorkDir(job.ID)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(jobWorkDir) }()

	// Build into a per-job PKGDIR so artifact collection sees only this build's
	// packages (the host /var/cache/binpkgs accumulates across jobs).
	pkgDir := filepath.Join(jobWorkDir, "binpkgs")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return err
	}

	pkgAtom, env := lb.prepareNativeBuildEnv(job)
	env = append(env, "PKGDIR="+pkgDir)

	if err := lb.runNativeBuild(job, pkgAtom, env, jobWorkDir); err != nil {
		return err
	}

	// Copy every produced gpkg into the artifact dir (category preserved), then
	// pick the requested package as primary.
	return lb.collectAndUploadArtifact(job, pkgDir)
}

// prepareNativeBuildEnv prepares the package atom and environment variables.
func (lb *LocalBuilder) prepareNativeBuildEnv(job *BuildJob) (string, []string) {
	req := job.Request
	pkgAtom := req.PackageName
	if req.Version != "" {
		pkgAtom = fmt.Sprintf("=%s-%s", req.PackageName, req.Version)
	}

	env := os.Environ()

	for k, v := range req.Environment {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	if lb.cfg != nil {
		pkgEnv := lb.pkgMgr.GetEnvVars(lb.cfg)
		for k, v := range pkgEnv {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
	}

	if len(req.UseFlags) > 0 {
		useFlags := buildUseFlagsString(req.UseFlags)
		env = append(env, fmt.Sprintf("USE=%s", useFlags))
	}

	return pkgAtom, env
}

// runNativeBuild executes the native build command.
func (lb *LocalBuilder) runNativeBuild(job *BuildJob, pkgAtom string, env []string, workDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	buildCmd := lb.pkgMgr.BuildCommand(pkgAtom, nil)
	cmd := exec.CommandContext(ctx, buildCmd[0], buildCmd[1:]...)
	cmd.Env = env
	cmd.Dir = workDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("build failed to start: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		job.appendLog(sc.Text() + "\n")
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}
	return nil
}

// sendNotification sends build completion notification.
func (lb *LocalBuilder) sendNotification(job *BuildJob) {
	if lb.notifier == nil {
		return
	}

	duration := job.EndTime.Sub(job.StartTime)
	notify := &notification.BuildNotification{
		JobID:       job.ID,
		PackageName: job.Request.PackageName,
		Version:     job.Request.Version,
		Status:      job.Status,
		StartTime:   job.StartTime,
		EndTime:     job.EndTime,
		Duration:    duration.String(),
		BuildLog:    job.Log,
		Error:       job.Error,
		ArtifactURL: job.ArtifactURL,
	}

	if err := lb.notifier.Notify(notify); err != nil {
		log.Printf("Failed to send notification for job %s: %v", job.ID, err)
	}
}

// GetArtifactPath returns the local file path of the artifact for a job.
// Returns empty string if job not found or artifact not available.
func (lb *LocalBuilder) GetArtifactPath(jobID string) (string, error) {
	lb.jobsMutex.RLock()
	job, exists := lb.jobs[jobID]
	lb.jobsMutex.RUnlock()

	if !exists {
		return "", fmt.Errorf("job not found: %s", jobID)
	}

	status, artifactURL := job.snapshot()
	if status != "success" {
		return "", fmt.Errorf("job not completed successfully: status=%s", status)
	}

	if artifactURL == "" {
		return "", fmt.Errorf("no artifact available for job: %s", jobID)
	}

	// Check if ArtifactURL is a local file path
	if _, err := os.Stat(artifactURL); err != nil {
		return "", fmt.Errorf("artifact file not found: %s", artifactURL)
	}

	return artifactURL, nil
}

// GetArtifactPathByRel returns the absolute path of one produced artifact,
// validated against the job's recorded artifact list (no path traversal).
func (lb *LocalBuilder) GetArtifactPathByRel(jobID, rel string) (string, error) {
	lb.jobsMutex.RLock()
	job, exists := lb.jobs[jobID]
	lb.jobsMutex.RUnlock()
	if !exists {
		return "", fmt.Errorf("job not found: %s", jobID)
	}
	for _, known := range job.artifactsSnapshot() {
		if known == rel {
			p := filepath.Join(lb.artifactDir, rel)
			if _, err := os.Stat(p); err != nil {
				return "", fmt.Errorf("artifact file not found: %s", rel)
			}
			return p, nil
		}
	}
	return "", fmt.Errorf("artifact %q not produced by job %s", rel, jobID)
}

// ArtifactInfo contains metadata about a build artifact.
type ArtifactInfo struct {
	JobID       string `json:"job_id"`
	FileName    string `json:"file_name"`
	FilePath    string `json:"file_path"`
	FileSize    int64  `json:"file_size"`
	PackageName string `json:"package_name"`
	Version     string `json:"version"`
}

// GetArtifactInfo returns metadata about the artifact for a job.
func (lb *LocalBuilder) GetArtifactInfo(jobID string) (*ArtifactInfo, error) {
	lb.jobsMutex.RLock()
	job, exists := lb.jobs[jobID]
	lb.jobsMutex.RUnlock()

	if !exists {
		return nil, fmt.Errorf("job not found: %s", jobID)
	}

	status, artifactURL := job.snapshot()
	if status != "success" {
		return nil, fmt.Errorf("job not completed successfully: status=%s", status)
	}

	if artifactURL == "" {
		return nil, fmt.Errorf("no artifact available for job: %s", jobID)
	}

	// Get file info
	fileInfo, err := os.Stat(artifactURL)
	if err != nil {
		return nil, fmt.Errorf("artifact file not found: %s", artifactURL)
	}

	return &ArtifactInfo{
		JobID:       jobID,
		FileName:    filepath.Base(artifactURL),
		FilePath:    artifactURL,
		FileSize:    fileInfo.Size(),
		PackageName: job.Request.PackageName,
		Version:     job.Request.Version,
	}, nil
}

// verifyInstallNative installs one digest-bound generation from a fresh local
// PKGDIR. Remote fetches finish before emerge starts, so Portage cannot reuse a
// package downloaded by the earlier unsigned gate or contact another binhost.
func (lb *LocalBuilder) verifyInstallNative(request VerifyInstallRequest) (string, error) {
	root, err := os.MkdirTemp("", "pe-verify-root")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(root) }()

	seed := nativeVerifySeedCommand(root, request.RequireSignature)
	if out, err := exec.Command("bash", "-c", seed).CombinedOutput(); err != nil {
		return string(out), fmt.Errorf("failed to seed verify root: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := removeBuiltPackagesFromVDB(root, request.BuiltPackages); err != nil {
		return "", fmt.Errorf("failed to prepare baseline package database: %w", err)
	}

	pkgDir := filepath.Join(root, "var", "cache", "binpkgs")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return "", err
	}
	if err := prefetchVerificationGeneration(request.BinhostURL, pkgDir, request.Artifacts); err != nil {
		return "", fmt.Errorf("prefetch exact %s generation: %w", request.Generation, err)
	}

	gpgHome := ""
	fingerprint := ""
	if request.RequireSignature {
		gpgHome = filepath.Join(root, "etc", "portage", "gnupg")
		fingerprint, err = installVerifierPublicKey(gpgHome, []byte(request.GPGPubkey))
		if err != nil {
			return "", fmt.Errorf("failed to install signer public key in verify root: %w", err)
		}
		if !strings.HasSuffix(strings.ToUpper(fingerprint), strings.ToUpper(request.ExpectedKeyID)) {
			return "", fmt.Errorf("signer public-key fingerprint %s does not match expected key ID %s", fingerprint, request.ExpectedKeyID)
		}
		for _, artifact := range request.Artifacts {
			file := filepath.Join(pkgDir, filepath.FromSlash(artifact.RelativePath))
			if err := (signing.GPG{Home: gpgHome, KeyID: request.ExpectedKeyID}).VerifyGPKG(file); err != nil {
				return "", fmt.Errorf("signed artifact %q failed independent GPG verification: %w", artifact.RelativePath, err)
			}
		}
		if out, err := exec.Command("chown", "-R", "nobody:nobody", gpgHome).CombinedOutput(); err != nil {
			return string(out), fmt.Errorf("failed to assign verify keyring ownership: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}

	var audit strings.Builder
	fmt.Fprintf(&audit, "verification generation=%s signature_required=%t artifacts=%d",
		request.Generation, request.RequireSignature, len(request.Artifacts))
	if fingerprint != "" {
		fmt.Fprintf(&audit, " signer_fingerprint=%s", fingerprint)
	}
	audit.WriteByte('\n')
	for _, artifact := range request.Artifacts {
		fmt.Fprintf(&audit, "verified input path=%s size=%d sha256=%s\n",
			artifact.RelativePath, artifact.Size, artifact.SHA256)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	args := []string{
		"--root=" + root,
		"--usepkgonly=y",
		"--getbinpkg=n",
		"--oneshot",
		"--color=n", "-q",
		"--",
		request.PackageName,
	}
	cmd := exec.CommandContext(ctx, "emerge", args...)
	feature := "-binpkg-request-signature"
	if request.RequireSignature {
		feature = "binpkg-request-signature"
	}
	env := append(os.Environ(),
		"PKGDIR="+pkgDir,
		"DISTDIR="+filepath.Join(root, "var", "cache", "distfiles"),
		"PORTAGE_TMPDIR="+filepath.Join(root, "var", "tmp"),
		"FEATURES="+feature,
		"PORTAGE_BINHOST=",
	)
	if gpgHome != "" {
		env = append(env, "BINPKG_GPG_VERIFY_GPG_HOME="+gpgHome)
	}
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	audit.Write(out)
	if err != nil {
		return audit.String(), fmt.Errorf("native install verification failed: %w", err)
	}
	if request.RequireSignature {
		if err := assertVerifierKeyring(gpgHome, fingerprint); err != nil {
			return audit.String(), err
		}
	}
	if err := verifyLocalArtifactSet(pkgDir, request.Artifacts); err != nil {
		return audit.String(), fmt.Errorf("post-install artifact proof failed: %w", err)
	}
	return audit.String(), nil
}

// nativeVerifySeedCommand seeds only the installed-package database required
// for dependency resolution. It deliberately does not copy host binpkg
// caches, configured binhosts, or Gentoo release-key bundles.
func nativeVerifySeedCommand(root string, requireSignature bool) string {
	seed := fmt.Sprintf(
		"chmod 0755 %[1]s && "+
			"mkdir -p %[1]s/etc/portage %[1]s/var/db %[1]s/var/cache/distfiles %[1]s/var/tmp && "+
			"cp -a /var/db/pkg %[1]s/var/db/pkg",
		root)
	if requireSignature {
		seed += fmt.Sprintf("; install -d -m 0700 %[1]s/etc/portage/gnupg", root)
	}
	return seed
}

// installVerifierPublicKey creates a job-local trust store containing exactly
// one isolated-signer public key and returns its primary fingerprint.
func installVerifierPublicKey(gpgHome string, publicKey []byte) (string, error) {
	if len(publicKey) == 0 {
		return "", fmt.Errorf("signer public key is empty")
	}
	if len(publicKey) > 1<<20 {
		return "", fmt.Errorf("signer public key exceeds 1 MiB")
	}
	if err := os.MkdirAll(gpgHome, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(gpgHome, 0o700); err != nil {
		return "", err
	}
	keyFile, err := os.CreateTemp(filepath.Dir(gpgHome), ".signer-public-*.asc")
	if err != nil {
		return "", err
	}
	keyPath := keyFile.Name()
	defer func() { _ = os.Remove(keyPath) }()
	if err := keyFile.Chmod(0o600); err != nil {
		_ = keyFile.Close()
		return "", err
	}
	if _, err := keyFile.Write(publicKey); err != nil {
		_ = keyFile.Close()
		return "", err
	}
	if err := keyFile.Close(); err != nil {
		return "", err
	}

	show := exec.Command("gpg", "--homedir", gpgHome, "--batch", "--with-colons",
		"--import-options", "show-only", "--import", keyPath)
	output, err := show.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspect public key: %w: %s", err, strings.TrimSpace(string(output)))
	}
	fingerprints := parsePrimaryFingerprints(output)
	if len(fingerprints) != 1 {
		return "", fmt.Errorf("public key bundle must contain exactly one primary key, found %d", len(fingerprints))
	}

	importKey := exec.Command("gpg", "--homedir", gpgHome, "--batch", "--import", keyPath)
	if output, err := importKey.CombinedOutput(); err != nil {
		return "", fmt.Errorf("import public key: %w: %s", err, strings.TrimSpace(string(output)))
	}
	ownerTrust := exec.Command("gpg", "--homedir", gpgHome, "--batch", "--import-ownertrust")
	ownerTrust.Stdin = strings.NewReader(fingerprints[0] + ":6:\n")
	if output, err := ownerTrust.CombinedOutput(); err != nil {
		return "", fmt.Errorf("trust public key: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return fingerprints[0], nil
}

// removeBuiltPackagesFromVDB turns the copied host VDB into a template
// baseline. Every package produced by this job is removed so emerge must fetch
// it from the tested binhost, while image-contract packages such as glibc stay
// satisfied by the baseline.
func removeBuiltPackagesFromVDB(root string, cpvs []string) error {
	for _, cpv := range cpvs {
		if !atomPattern.MatchString(cpv) || strings.Count(cpv, "/") != 1 {
			return fmt.Errorf("invalid built package CPV %q", cpv)
		}
		parts := strings.SplitN(cpv, "/", 2)
		target := filepath.Join(root, "var", "db", "pkg", parts[0], parts[1])
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	return nil
}

// VerifyInstall confirms the exact expected generation installs through a
// seeded throwaway native --root.
func (lb *LocalBuilder) VerifyInstall(request VerifyInstallRequest) (string, error) {
	if err := validateVerifyInstallRequest(request); err != nil {
		return "", err
	}
	return lb.verifyInstallNative(request)
}
