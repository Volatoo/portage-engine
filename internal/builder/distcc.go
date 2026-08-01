package builder

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/slchris/portage-engine/internal/distcc"
)

// configureDistCC writes a Portage package.env entry only for locally
// reviewed package atoms. Phase-aware compiler wrappers keep configure, link,
// test, and every non-object compiler invocation local. Packages outside the
// mapping (including small packages and Rust/Go/Java) never see the wrappers.
// No lease credential is exposed to an ebuild process.
func (be *BuildExecutor) configureDistCC(
	configRoot string,
	bundle *ConfigBundle,
	job *BuildJob,
) error {
	if job == nil || job.Request == nil || job.Request.CompileLease == nil {
		return nil
	}
	lease := job.Request.CompileLease
	eligible := make([]string, 0, len(bundle.Packages.Packages))
	for _, pkg := range bundle.Packages.Packages {
		if be.opts.DistCCPolicy.Allowed(pkg.Atom) {
			eligible = append(eligible, pkg.Atom)
		}
	}
	if len(eligible) == 0 {
		if lease.FallbackPolicy == distcc.FallbackBlocked {
			return fmt.Errorf("distcc is required but no reviewed C/C++ package is eligible")
		}
		job.appendLog("[distcc] no reviewed C/C++ package in this request; compiling locally\n")
		return nil
	}
	if err := be.opts.DistCCPolicy.Authorize(
		eligible[0], job.Request.ProjectID, job.Request.AttemptID,
		job.Request.AttemptFence, lease, time.Now(),
	); err != nil {
		if lease.FallbackPolicy == distcc.FallbackLocal {
			job.appendLog("[distcc] lease unavailable; controlled local fallback\n")
			return nil
		}
		return fmt.Errorf("distcc blocked by lease policy: %w", err)
	}
	portageDir := filepath.Join(configRoot, "etc", "portage")
	envDir := filepath.Join(portageDir, "env")
	packageEnvDir := filepath.Join(portageDir, "package.env")
	wrapperDir := filepath.Join(configRoot, "var", "lib", "portage-engine", "distcc-wrappers")
	if err := os.MkdirAll(envDir, 0o755); err != nil { // #nosec G301 -- Portage reads this builder-owned tree.
		return fmt.Errorf("create distcc Portage env directory: %w", err)
	}
	if err := os.MkdirAll(packageEnvDir, 0o755); err != nil { // #nosec G301 -- Portage reads this builder-owned tree.
		return fmt.Errorf("create distcc package.env directory: %w", err)
	}
	if err := os.MkdirAll(wrapperDir, 0o755); err != nil { // #nosec G301 -- Portage executes these builder-owned wrappers.
		return fmt.Errorf("create distcc compiler wrapper directory: %w", err)
	}
	ccWrapper := filepath.Join(wrapperDir, "cc")
	cxxWrapper := filepath.Join(wrapperDir, "cxx")
	metricsFile := filepath.Join(wrapperDir, "observations.tsv")
	logDir := filepath.Join(wrapperDir, "logs")
	if err := os.MkdirAll(logDir, 0o1777); err != nil { // #nosec G301 -- job-private sticky dir for per-invocation distcc logs.
		return fmt.Errorf("create distcc invocation log directory: %w", err)
	}
	if err := os.Chmod(logDir, 0o1777); err != nil { // #nosec G302 -- Portage helpers must create isolated per-invocation logs.
		return fmt.Errorf("permit distcc invocation logs: %w", err)
	}
	if err := os.WriteFile(metricsFile, nil, 0o666); err != nil { // #nosec G306 -- bounded, non-secret compile counters written by Portage helpers.
		return fmt.Errorf("create distcc observation file: %w", err)
	}
	if err := os.Chmod(metricsFile, 0o666); err != nil { // #nosec G302 -- job-private root; Portage compiler helpers must append counters.
		return fmt.Errorf("permit distcc observation append: %w", err)
	}
	if err := writeDistCCCompilerWrapper(
		ccWrapper, lease.Pool.CHOST+"-gcc", lease.FallbackPolicy,
	); err != nil {
		return err
	}
	if err := writeDistCCCompilerWrapper(
		cxxWrapper, lease.Pool.CHOST+"-g++", lease.FallbackPolicy,
	); err != nil {
		return err
	}
	const envName = "portage-engine-distcc-lease"
	envContent := strings.Join([]string{
		`FEATURES="-distcc -distcc-pump"`,
		fmt.Sprintf(`CC="%s"`, ccWrapper),
		fmt.Sprintf(`CXX="%s"`, cxxWrapper),
		fmt.Sprintf(`PORTAGE_ENGINE_DISTCC_METRICS="%s"`, metricsFile),
		fmt.Sprintf(`PORTAGE_ENGINE_DISTCC_LOG_DIR="%s"`, logDir),
		fmt.Sprintf(`DISTCC_HOSTS="%s/%d"`, lease.WorkerEndpoint, lease.Slots),
		// distcc's implicit retry is disabled so the phase-aware wrapper can
		// distinguish and count the reviewed local fallback policy.
		`DISTCC_FALLBACK="0"`,
		// Verbose events stay in per-invocation job-local log files and are
		// reduced to bounded counters before leaving the builder.
		`DISTCC_VERBOSE="1"`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(envDir, envName), []byte(envContent), 0o644); err != nil { // #nosec G306 -- Portage must read the non-secret environment.
		return fmt.Errorf("write distcc Portage environment: %w", err)
	}
	sort.Strings(eligible)
	lines := make([]string, 0, len(eligible))
	for _, atom := range eligible {
		lines = append(lines, atom+" "+envName)
	}
	if err := os.WriteFile(
		filepath.Join(packageEnvDir, "90-portage-engine-distcc"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644,
	); err != nil { // #nosec G306 -- Portage must read the reviewed package mapping.
		return fmt.Errorf("write distcc package allowlist mapping: %w", err)
	}
	job.appendLog(fmt.Sprintf(
		"[distcc] exact pool %s lease fence %d enabled for %d reviewed package(s); pump disabled; fallback=%s\n",
		lease.PoolID, lease.Fence, len(eligible), lease.FallbackPolicy,
	))
	return nil
}

// writeDistCCCompilerWrapper permits only C/C++ object compilation during the
// Portage compile phase to enter distcc. Configure probes, preprocessing,
// assembly, linking, tests, and every other phase execute the exact local
// compiler directly. distcc pump is never invoked.
func writeDistCCCompilerWrapper(path, compiler, fallbackPolicy string) error {
	localFallback := "false"
	if fallbackPolicy == distcc.FallbackLocal {
		localFallback = "true"
	}
	content := fmt.Sprintf(`#!/bin/sh
set -u
record_observation() {
  printf '%%s\t%%s\t%%s\t1\n' "$1" "$2" "$3" >> "$PORTAGE_ENGINE_DISTCC_METRICS"
}
network_payload_bytes() {
  awk '
    / send [0-9]+ byte file / {
      for (i = 1; i <= NF; i++) if ($i == "send") total += $(i + 1)
    }
    / received [0-9]+ bytes to file/ {
      for (i = 1; i <= NF; i++) if ($i == "received") total += $(i + 1)
    }
    END { print total + 0 }
  ' "$1"
}
remote=false
if [ "${EBUILD_PHASE:-}" = "compile" ]; then
  for arg in "$@"; do
    if [ "$arg" = "-c" ]; then
      remote=true
      break
    fi
  done
fi
if [ "$remote" = "true" ]; then
	log_file=$(mktemp "${PORTAGE_ENGINE_DISTCC_LOG_DIR}/invocation.XXXXXX") || exit 70
	DISTCC_LOG="$log_file" DISTCC_FALLBACK=0 DISTCC_SKIP_LOCAL_RETRY=1 \
	  /usr/bin/distcc /usr/bin/%s "$@"
	remote_status=$?
	bytes=$(network_payload_bytes "$log_file")
	if [ "$remote_status" -eq 0 ]; then
	  record_observation remote '' "$bytes"
	  exit 0
	fi
	if [ "%s" = "true" ]; then
	  /usr/bin/%s "$@"
	  local_status=$?
	  record_observation fallback remote-compile "$bytes"
	  if [ "$local_status" -eq 0 ]; then
	    record_observation local '' 0
	  else
	    record_observation local unknown 0
	  fi
	  exit "$local_status"
	fi
	record_observation remote remote-compile "$bytes"
	exit "$remote_status"
fi
	/usr/bin/%s "$@"
	local_status=$?
	if [ "$local_status" -eq 0 ]; then
	  record_observation local '' 0
	else
	  record_observation local unknown 0
	fi
	exit "$local_status"
`, compiler, localFallback, compiler, compiler)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { // #nosec G306 -- executable builder-owned compiler wrapper.
		return fmt.Errorf("write distcc compiler wrapper: %w", err)
	}
	return nil
}

// recordDistCCReport converts job-private wrapper events into a bounded report
// returned to the scheduler. The raw compiler paths and distcc logs never
// leave the disposable builder.
func (be *BuildExecutor) recordDistCCReport(configRoot string, job *BuildJob) {
	if job == nil || job.Request == nil || job.Request.CompileLease == nil {
		return
	}
	file := filepath.Join(configRoot, "var", "lib", "portage-engine", "distcc-wrappers", "observations.tsv")
	handle, err := os.Open(file) // #nosec G304 -- path is confined to the builder-created job config root.
	if err != nil {
		job.appendLog("[distcc] observation report unavailable\n")
		return
	}
	defer func() { _ = handle.Close() }()
	type key struct{ outcome, reason string }
	aggregated := make(map[key]distcc.Observation)
	scanner := bufio.NewScanner(handle)
	lines := 0
	for scanner.Scan() {
		lines++
		if lines > 1_000_000 {
			job.appendLog("[distcc] observation report exceeded event limit\n")
			return
		}
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 4 {
			continue
		}
		bytes, bytesErr := strconv.ParseInt(fields[2], 10, 64)
		count, countErr := strconv.ParseInt(fields[3], 10, 64)
		observation := distcc.Observation{
			Outcome: fields[0], FailureReason: fields[1],
			NetworkBytes: bytes, Count: count,
		}
		if bytesErr != nil || countErr != nil || observation.Validate() != nil {
			continue
		}
		item := aggregated[key{observation.Outcome, observation.FailureReason}]
		item.Outcome, item.FailureReason = observation.Outcome, observation.FailureReason
		item.NetworkBytes += observation.NetworkBytes
		item.Count += observation.Count
		aggregated[key{observation.Outcome, observation.FailureReason}] = item
	}
	if err := scanner.Err(); err != nil {
		job.appendLog("[distcc] observation report read failed\n")
		return
	}
	report := distcc.Report{Observations: make([]distcc.Observation, 0, len(aggregated))}
	for _, observation := range aggregated {
		report.Observations = append(report.Observations, observation)
	}
	sort.Slice(report.Observations, func(i, j int) bool {
		if report.Observations[i].Outcome == report.Observations[j].Outcome {
			return report.Observations[i].FailureReason < report.Observations[j].FailureReason
		}
		return report.Observations[i].Outcome < report.Observations[j].Outcome
	})
	if err := report.Validate(); err != nil {
		job.appendLog("[distcc] observation report rejected\n")
		return
	}
	job.setMetadata(distcc.MetadataReportKey, report)
}
