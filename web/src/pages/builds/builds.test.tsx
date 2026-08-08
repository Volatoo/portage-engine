import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { describe, expect, it } from 'vitest';

import type { BuildStatus } from '../../api/types';
import { UNRESOLVED_IDENTITY } from '../../app/capabilities';
import type { IdentityContext } from '../../app/capabilities';
import { CAPABILITY_SYSTEM_ADMIN, CONSOLE_ROUTES } from '../../app/routes';
import type { ConsoleRoute } from '../../app/routes';
import type { BootPayload } from '../../boot/payload';
import { MessagesProvider } from '../../i18n/context';
import { BuildDetailPage } from './BuildDetailPage';
import { BuildsPage } from './BuildsPage';
import { canMaintainJob } from './data';
import { PAGE_SIZE, matchesQuery, paginate } from './filter';
import { FailureReport, PipelineRow, StageCards } from './jobstate';
import { logFault, readLogDocument } from './logdoc';
import { LogPane } from './logpane';
import { ConfirmDialog, StateBox, StatusBadge } from './parts';
import { findFilter, findRemedy, lastFailureLine, stageStates } from './stages';

/**
 * The measured defects of the console these four routes replace, each pinned by
 * the reading it produced rather than by the code path it took.
 *
 * Static markup rather than a mounted tree, for the same reason the shell tests
 * are: the frame under test is the one the reader gets, and everything asserted
 * here is decided before any effect runs.
 */

function paint(node: React.ReactNode, at = '/build/j1'): string {
  return renderToStaticMarkup(
    <MessagesProvider lang="en">
      <MemoryRouter initialEntries={[at]}>{node}</MemoryRouter>
    </MessagesProvider>,
  );
}

const BOOT: BootPayload = {
  lang: 'en',
  html_lang: 'en',
  auth_enabled: true,
  oidc_enabled: false,
  local_login_enabled: true,
  identity_providers: [],
  principal: null,
  step_up_method: 'local',
  route: { name: 'builds', path: '/ui/builds', job_id: '', instance_id: '', user_code: '' },
  asset_base: '/static/ui/',
};

function routeNamed(name: string): ConsoleRoute {
  const found = CONSOLE_ROUTES.find((route) => route.name === name);
  if (found === undefined) {
    throw new Error(`no route named ${name}`);
  }
  return found;
}

const ADMIN: IdentityContext = {
  granted: new Set([CAPABILITY_SYSTEM_ADMIN]),
  displayName: 'operator@example.test',
  projects: [{ project_id: 'p1', project_name: 'Default', role: 'owner' }],
  resolved: true,
  failure: null,
};

/**
 * A real failed publish, in the shape the builder writes it: every event goes
 * through one appender, so the failure and the byte gate that passed just before
 * it are the same shape and the failure is NOT the last line.
 */
const PUBLISH_LOG = [
  '[queued] job accepted',
  '[provision] creating instance',
  '[deploy] builder online',
  '[build] submitting emerge',
  '[collect] artifact collected',
  '[verify] isolated install passed',
  '[sign] detached signature written',
  '[publish] artifact promotion failed: generation repository identity and revision or digest are required',
  '[publish] artifact byte gate passed: 41231 bytes',
  '[cleanup] instance destroyed',
].join('\n');

const FAILED_JOB: BuildStatus = {
  job_id: 'j1',
  status: 'failed',
  package_name: 'app-editors/vim',
  version: '9.1',
  arch: 'amd64',
  created_at: '2026-07-31T10:00:00Z',
  updated_at: '2026-07-31T10:31:12Z',
  project_id: 'p1',
  failed_stage: 'publish',
  error:
    'artifact promotion failed: generation repository identity and revision or digest are required',
};

describe('a failed build explains itself', () => {
  it('leads with what failed, where, and the control plane words for why', () => {
    const markup = paint(
      <FailureReport job={FAILED_JOB} logText={PUBLISH_LOG} canReachAdmin={false} />,
    );
    expect(markup).toContain('Build failed during Publish');
    expect(markup).toContain('generation repository identity and revision or digest are required');
  });

  it('carries the failing stage error line, not the passing line that came after it', () => {
    // The defect this refuses: the failing stage showed "[publish] artifact byte
    // gate passed…", which is a passing message on the card for the stage that
    // failed.
    expect(lastFailureLine(PUBLISH_LOG, 'publish')).toBe(
      '[publish] artifact promotion failed: generation repository identity and revision or digest are required',
    );
    const markup = paint(
      <StageCards
        stages={[
          { id: 'publish', line_count: 2, last_message: '[publish] artifact byte gate passed' },
        ]}
        states={stageStates(FAILED_JOB, PUBLISH_LOG)}
        job={FAILED_JOB}
        logText={PUBLISH_LOG}
      />,
    );
    expect(markup).toContain('artifact promotion failed');
    expect(markup).not.toContain('byte gate passed');
    // The failing card is told apart from the eight beside it by a class the
    // stylesheet washes AND by its own state word.
    expect(markup).toContain('stage-log-card failed');
    expect(markup).toContain('failed');
  });

  it('points a catalog failure at the catalog, and a storage one at its deployment key', () => {
    const catalog = findRemedy(FAILED_JOB.error ?? '');
    expect(catalog).toEqual({
      kind: 'console',
      href: '/image-factory',
      fieldKey: 'factory.catalog',
      sectionKey: 'nav.factory',
    });
    expect(findRemedy('artifact deletion refused: S3 object deletion is disabled')).toEqual({
      kind: 'deployment',
      env: 'STORAGE_S3_ALLOW_DELETE',
    });
    expect(findRemedy('emerge failed with exit status 1')).toBeNull();
  });

  it('names the destination without linking to it until IAM grants the capability', () => {
    const closed = paint(
      <FailureReport job={FAILED_JOB} logText={PUBLISH_LOG} canReachAdmin={false} />,
    );
    expect(closed).toContain('Active Catalog');
    expect(closed).not.toContain('href');

    const granted = paint(<FailureReport job={FAILED_JOB} logText={PUBLISH_LOG} canReachAdmin />);
    expect(granted).toContain('href="/image-factory"');
  });

  it('says nothing at all about a job that did not fail', () => {
    const passing: BuildStatus = {
      ...FAILED_JOB,
      status: 'completed',
      failed_stage: '',
      error: '',
    };
    expect(paint(<FailureReport job={passing} logText="" canReachAdmin />)).toBe('');
  });
});

describe('the first screen of a build detail', () => {
  it('paints no pipeline and no verdict before the record has answered', () => {
    const markup = paint(
      <BuildDetailPage
        route={routeNamed('build-detail')}
        boot={{ ...BOOT, route: { ...BOOT.route, job_id: 'j1' } }}
        onLanguageChange={() => undefined}
      />,
    );
    // No chips, no stage words, no meta tiles: the frame before the first
    // answer says it is loading and claims nothing about a job.
    expect(markup).not.toContain('pipe-chip');
    expect(markup).not.toContain('stage-log-card');
    expect(markup).toContain('data-state="loading"');
    expect(markup).toContain('Loading…');
    // And the sentence that belongs to a failed read is not on a request that
    // has not failed.
    expect(markup).not.toContain('could not be read');
    // The job id is on the page from the first frame, because the server
    // resolved it out of the URL before the bundle ran.
    expect(markup).toContain('j1');
  });

  it('keeps every write off the page until IAM answers for this job', () => {
    const markup = paint(
      <BuildDetailPage
        route={routeNamed('build-detail')}
        boot={{ ...BOOT, route: { ...BOOT.route, job_id: 'j1' } }}
        onLanguageChange={() => undefined}
      />,
    );
    for (const control of ['Cancel Job', 'Retry Job', 'Delete Job']) {
      expect(markup).not.toContain(control);
    }
    // The two that only read are there, so the head is not simply empty.
    expect(markup).toContain('View Logs');
    expect(markup).toContain('Refresh');
  });
});

describe('the pipeline never invents a reading', () => {
  it('has no states to draw when there is no job record', () => {
    // The defect: a failed detail fetch was swallowed and the row was painted
    // from `lastDetail || {}`, so a job whose record 404s rendered nine chips
    // with Queued marked running and a pulsing dot.
    expect(stageStates(null, PUBLISH_LOG)).toEqual([]);
  });

  it('marks the stage that failed, and never queues anything behind it', () => {
    const states = stageStates(FAILED_JOB, PUBLISH_LOG);
    expect(states[7]).toBe('failed');
    expect(states.slice(0, 7).every((state) => state === 'done')).toBe(true);
    // Release runs whatever happened, so it is read from the log and not from
    // its position after the stage that stopped the job.
    expect(states[8]).toBe('done');
    expect(states).not.toContain('pending');
  });

  it('reads a stage as never started when its marker never reached the log', () => {
    const early: BuildStatus = { ...FAILED_JOB, failed_stage: 'verify' };
    const states = stageStates(early, '[queued] job accepted\n[provision] creating instance');
    expect(states[5]).toBe('failed');
    expect(states[3]).toBe('skipped');
    expect(states[8]).toBe('skipped');
  });

  it('writes a word beside every chip, so the state is never the dot alone', () => {
    const markup = paint(<PipelineRow states={stageStates(FAILED_JOB, PUBLISH_LOG)} />);
    expect(markup).toContain('pipe-state');
    expect(markup).toContain('failed');
    expect(markup).toContain('completed');
  });
});

describe('the log pane does not lie', () => {
  const stages = [{ id: 'publish', line_count: 2628 }];

  it('calls a pane holding nothing under a chip claiming lines an error', () => {
    const log = readLogDocument({ job_id: 'j1', logs: '', bytes: 270_000, stages });
    expect(logFault(log, findFilter('all'), 0, null)).toEqual({ kind: 'blank', counted: 2628 });
  });

  it('calls a stage chip that counts lines the pane does not hold an error too', () => {
    const log = readLogDocument({
      job_id: 'j1',
      logs: '[build] one line only',
      bytes: 21,
      stages,
    });
    expect(logFault(log, findFilter('publish'), 0, null)).toEqual({
      kind: 'blank',
      counted: 2628,
    });
  });

  it('calls a job that produced no output empty, which is not an error', () => {
    const log = readLogDocument({ job_id: 'j1', logs: '', bytes: 0, stages: [] });
    expect(logFault(log, findFilter('all'), 0, null)).toBeNull();
  });

  it('keeps a failed fetch as its own fault, with the server sentence on it', () => {
    const log = readLogDocument(null);
    expect(logFault(log, findFilter('all'), 0, 'build job not found')).toEqual({
      kind: 'load',
      message: 'build job not found',
    });
  });

  it('offers a retry and the full log rather than an empty box', () => {
    const markup = paint(
      <LogPane
        log={readLogDocument({ job_id: 'j1', logs: '', bytes: 270_000, stages })}
        activeFilter="all"
        jobID="j1"
        loadError={null}
        onRetry={() => undefined}
      />,
    );
    expect(markup).toContain('rendered none of them');
    expect(markup).toContain('Retry');
    expect(markup).toContain('href="/logs/j1"');
  });

  it('renders the log it was given, and says so when a filter selects none of it', () => {
    const log = readLogDocument({
      job_id: 'j1',
      logs: PUBLISH_LOG,
      bytes: PUBLISH_LOG.length,
      stages: [{ id: 'publish', line_count: 2 }],
    });
    const all = paint(
      <LogPane
        log={log}
        activeFilter="all"
        jobID="j1"
        loadError={null}
        onRetry={() => undefined}
      />,
    );
    expect(all).toContain('artifact promotion failed');
    expect(all).not.toContain('rendered none of them');

    const queued = paint(
      <LogPane
        log={log}
        activeFilter="policy"
        jobID="j1"
        loadError={null}
        onRetry={() => undefined}
      />,
    );
    // A filter that selects nothing out of a log that exists is a different
    // sentence from a job that has not logged anything yet.
    expect(queued).toContain('no line in this log belongs to this stage');
  });
});

describe('the volume controls answer for what they hold', () => {
  const rows: BuildStatus[] = Array.from({ length: 60 }, (_unused, index) => ({
    ...FAILED_JOB,
    job_id: `job-${String(index)}`,
    package_name: index % 2 === 0 ? 'app-editors/vim' : 'sys-apps/portage',
  }));

  it('matches the columns the reader can see, including the id they pasted in', () => {
    expect(matchesQuery(FAILED_JOB, 'vim')).toBe(true);
    expect(matchesQuery(FAILED_JOB, 'AMD64')).toBe(true);
    expect(matchesQuery(FAILED_JOB, 'j1')).toBe(true);
    expect(matchesQuery(FAILED_JOB, 'emacs')).toBe(false);
    expect(matchesQuery(FAILED_JOB, '   ')).toBe(true);
  });

  it('clamps a page past the end of a list that shrank under it', () => {
    expect(paginate(rows, 3).first).toBe(2 * PAGE_SIZE + 1);
    const shrunk = paginate(rows.slice(0, 10), 3);
    expect(shrunk.page).toBe(1);
    expect(shrunk.rows).toHaveLength(10);
    expect(shrunk.pages).toBe(1);
  });
});

describe('destructive controls are not refresh', () => {
  it('states the number of records a bulk delete will remove', () => {
    const markup = paint(
      <ConfirmDialog
        open
        titleKey="builds.cleanup.confirm"
        detail="Failed job records to delete: 12"
        confirmLabel="Clean Up Failed"
        busy={false}
        onConfirm={() => undefined}
        onCancel={() => undefined}
      />,
    );
    expect(markup).toContain('Remove all failed job records?');
    expect(markup).toContain('Failed job records to delete: 12');
    // The confirming control wears the danger treatment and the dismissal does
    // not, so the two are not one decision made twice.
    expect(markup).toContain('btn danger');
    expect(markup).toContain('Cancel');
  });

  it('keeps the bulk delete off the page until IAM has granted the capability', () => {
    // Fail closed: never rendered and then removed, which is a control an
    // operator can see, click, and be refused by.
    const markup = paint(
      <BuildsPage route={routeNamed('builds')} boot={BOOT} onLanguageChange={() => undefined} />,
      '/builds',
    );
    expect(markup).not.toContain('Clean Up Failed');
    // And the seven columns are pinned before any payload arrives.
    expect(markup).toContain('data-cols="7"');
    expect(markup).toContain('Job ID');
    expect(markup).toContain('skeleton-row');
  });

  it('refuses a write to a job in a project this reader does not maintain', () => {
    const maintainer: IdentityContext = {
      ...ADMIN,
      granted: new Set(),
      projects: [{ project_id: 'p2', project_name: 'Other', role: 'maintainer' }],
    };
    expect(canMaintainJob(maintainer, 'p1')).toBe(false);
    expect(canMaintainJob(maintainer, 'p2')).toBe(true);
    expect(canMaintainJob({ ...maintainer, projects: [] }, 'p1')).toBe(false);
    // A viewer of the right project is still a viewer.
    expect(
      canMaintainJob(
        { ...maintainer, projects: [{ project_id: 'p1', project_name: 'D', role: 'viewer' }] },
        'p1',
      ),
    ).toBe(false);
    // A system administrator maintains every project, and an unresolved
    // identity maintains none.
    expect(canMaintainJob(ADMIN, 'p9')).toBe(true);
    expect(canMaintainJob(UNRESOLVED_IDENTITY, 'p1')).toBe(false);
  });
});

describe('states and status', () => {
  it('tells a filtered-empty list from an empty one', () => {
    const filtered = paint(
      <StateBox
        state={{ state: 'filtered-empty', query: 'emacs' }}
        emptyKey="builds.empty"
        filteredKey="builds.none"
      />,
    );
    expect(filtered).toContain('No job matches this filter.');
    expect(filtered).not.toContain('No builds yet.');
    expect(filtered).toContain('data-state="filtered-empty"');

    const empty = paint(<StateBox state={{ state: 'empty' }} emptyKey="builds.empty" />);
    expect(empty).toContain('No builds yet.');
  });

  it('frames the server sentence on an error, and offers the way back', () => {
    const markup = paint(
      <StateBox
        state={{ state: 'error', message: 'build job not found', status: 404 }}
        emptyKey="detail.norecord"
        lead="This job record could not be read, so nothing about its pipeline is known."
        onRetry={() => undefined}
      />,
    );
    expect(markup).toContain('nothing about its pipeline is known');
    expect(markup).toContain('build job not found');
    expect(markup).toContain('Retry');
    expect(markup).toContain('data-state="error"');
  });

  it('renders a status the vocabulary has never heard of, gray and by its own name', () => {
    const markup = paint(<StatusBadge token="a_state_shipped_after_this_console" />);
    expect(markup).toContain('status gray');
    expect(markup).toContain('a_state_shipped_after_this_console');
    // Never colour alone: the dot and the word are both always there.
    expect(markup).toContain('class="dot"');
  });
});
