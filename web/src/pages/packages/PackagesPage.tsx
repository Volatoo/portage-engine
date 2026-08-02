/**
 * The public package catalogue.
 *
 * Four things the page this replaces got wrong, and the shape each fix took.
 *
 * 1. The sync path — the one string on the page an operator has to copy into
 *    binrepos.conf — existed only as a `title` attribute on the profile cell.
 *    A `title` is not reachable by touch, is not reachable by keyboard, and is
 *    not read by most screen readers; it was invisible to everyone who did not
 *    hover a mouse over exactly that cell. It is a column now, in the server's
 *    own spelling: `sync_path` off `/api/public/binhosts`, joined by profile id,
 *    never reassembled here out of `binhost_path` and a guessed prefix.
 *
 * 2. Signature state was absent entirely. It still cannot be reported per
 *    artifact — `publicPackage` in internal/server/handlers_binhost.go carries
 *    name, version, arch, USE flags, profile, binhost path and download path,
 *    and nothing about signing — so the column says the catalogue does not
 *    report it rather than inferring "signed" from the deployment holding a
 *    key. The moment the handler emits `signed`, the column resolves it through
 *    the same status vocabulary every other badge in the console uses.
 *
 * 3. The count read "1 published packages". It goes through `Intl.PluralRules`
 *    now, and the number inside it through the app locale's own formatter.
 *
 * 4. Empty and filtered-empty were one sentence, and the filter used to label a
 *    response was read out of the inputs at render time — so a response that
 *    landed after the reader had typed something else was described by the
 *    something else. The filter travels with the answer it produced, and
 *    `resolveState` decides which of the two empties this is.
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useSearchParams } from 'react-router';

import type { ApiOutcome } from '../../api/client';
import { api } from '../../api/endpoints';
import { resolveState } from '../../api/state';
import type { BinhostProfile, PublicPackage, PublicPackageList } from '../../api/types';
import { isCompositionEnter } from '../../api/write';
import { useMessages } from '../../i18n/context';
import { StateBox } from '../anonymous/StateBox';
import { StatusBadge } from '../anonymous/StatusBadge';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import '../../styles/pages/packages.css';

/**
 * The window asked for. `handlePublicPackages` clamps a limit to 1..200 and
 * answers 400 outside it, so the page states the page size it wants rather than
 * inheriting a server default it would then have to guess at when paging.
 */
const PAGE_SIZE = 50;

/** What one request asked for. Travels with the answer it produced. */
interface PackageFilter {
  readonly q: string;
  readonly profileID: string;
  readonly offset: number;
}

interface PackageAnswer {
  readonly filter: PackageFilter;
  /** Which run of the request this answers, so a retry of the same filter counts. */
  readonly attempt: number;
  readonly outcome: ApiOutcome<PublicPackageList>;
}

/**
 * The filter as one string, which is what makes an empty answer "filtered".
 *
 * `resolveState` reads only whether this is empty, so the exact wording never
 * reaches a reader; what matters is that it is built from the request that
 * produced the response and not from whatever is in the inputs right now.
 */
function filterText(filter: PackageFilter): string {
  return [filter.q, filter.profileID].filter((part) => part !== '').join(' ');
}

export function PackagesPage() {
  const messages = useMessages();
  useDocumentTitle('title.packages');

  const [searchParams, setSearchParams] = useSearchParams();
  // Seeded once from the URL so a shared link opens on the search it names, then
  // owned by the inputs: re-reading the URL on every render would fight the
  // reader mid-word.
  const [query, setQuery] = useState(() => searchParams.get('q') ?? '');
  const [profileID, setProfileID] = useState(() => searchParams.get('profile_id') ?? '');
  const [filter, setFilter] = useState<PackageFilter>(() => ({
    q: searchParams.get('q') ?? '',
    profileID: searchParams.get('profile_id') ?? '',
    offset: 0,
  }));

  const [answer, setAnswer] = useState<PackageAnswer | null>(null);
  const [profiles, setProfiles] = useState<readonly BinhostProfile[]>([]);
  const [profilesError, setProfilesError] = useState<string | null>(null);
  // Bumped by the retry control. A filter that failed is the same filter, so
  // nothing about `filter` changes and the effect needs its own reason to run.
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    void api.publicBinhosts({ signal: controller.signal }).then((outcome) => {
      if (controller.signal.aborted) {
        return;
      }
      if (outcome.kind === 'ok') {
        setProfiles(outcome.value.binhosts);
        setProfilesError(null);
        return;
      }
      // Not fatal to the page: the catalogue still lists, and the sync column
      // falls back to the binhost path each package carries itself. It is said
      // out loud rather than swallowed, because a profile filter the reader
      // cannot see is a filter they cannot clear.
      setProfilesError('message' in outcome ? outcome.message : outcome.kind);
    });
    return () => {
      controller.abort();
    };
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    void api
      .publicPackages(
        { q: filter.q, profileID: filter.profileID, limit: PAGE_SIZE, offset: filter.offset },
        { signal: controller.signal },
      )
      .then((outcome) => {
        if (controller.signal.aborted) {
          return;
        }
        // The filter and the outcome are stored together, so nothing downstream
        // can describe this response with a filter that arrived after it.
        setAnswer({ filter, attempt, outcome });
      });
    return () => {
      controller.abort();
    };
  }, [filter, attempt]);

  // Derived, not stored. A "request in flight" flag written from inside the
  // effect is a second render pass and a second source of truth for one fact;
  // the fact itself is simply that the answer on hand is not the answer to the
  // request currently out.
  const pending = answer === null || answer.filter !== filter || answer.attempt !== attempt;
  const payload = answer?.outcome.kind === 'ok' ? answer.outcome.value : null;
  const rows = payload?.packages ?? [];
  const total = payload?.total ?? 0;

  // `count` is supplied only when a payload actually landed. Passing it as
  // `undefined` and passing nothing are the same value and not the same fact:
  // the resolver reads a present zero as "this answer holds no rows", which is
  // a claim no answer has made while one is still in flight.
  const resolved = resolveState<PublicPackageList>({
    outcome: pending ? null : (answer?.outcome ?? null),
    query: answer === null ? '' : filterText(answer.filter),
    ...(!pending && payload !== null ? { count: payload.packages.length } : {}),
  });

  /** profile id to the sync path the server publishes for it. */
  const syncPaths = useMemo(() => {
    const table = new Map<string, string>();
    for (const profile of profiles) {
      table.set(profile.profile_id, profile.sync_path);
    }
    return table;
  }, [profiles]);

  const apply = useCallback(
    (next: PackageFilter) => {
      setFilter(next);
      const params = new URLSearchParams();
      if (next.q !== '') {
        params.set('q', next.q);
      }
      if (next.profileID !== '') {
        params.set('profile_id', next.profileID);
      }
      // Replace rather than push: a search is a refinement of where the reader
      // already is, and pushing one entry per keystroke-then-submit turns Back
      // into a walk through their own typing.
      setSearchParams(params, { replace: true });
    },
    [setSearchParams],
  );

  const atStart = filter.offset === 0;
  const atEnd = filter.offset + PAGE_SIZE >= total;
  const start = total === 0 ? 0 : filter.offset + 1;
  const end = Math.min(filter.offset + PAGE_SIZE, total);

  return (
    <>
      <div className="public-head">
        <h1>{messages.t('packages.h1')}</h1>
        <p>{messages.t('packages.sub')}</p>
      </div>

      <div className="card">
        <div className="card-pad">
          <form
            className="public-search"
            onSubmit={(event) => {
              event.preventDefault();
              apply({ q: query.trim(), profileID, offset: 0 });
            }}
            // Both shipped languages are typed through an IME at least some of
            // the time, and the Enter that closes the candidate window is not a
            // submission. Checked at the form, so no individual control has to
            // remember.
            // The native event, because `isComposing` is a property of the DOM
            // event and React's synthetic keyboard event does not republish it.
            onKeyDown={(event) => {
              if (isCompositionEnter(event.nativeEvent)) {
                event.preventDefault();
              }
            }}
          >
            <div className="field">
              <label htmlFor="package-query">{messages.t('packages.search')}</label>
              <input
                id="package-query"
                type="text"
                autoComplete="off"
                spellCheck={false}
                placeholder={messages.t('packages.search.ph')}
                value={query}
                onChange={(event) => {
                  setQuery(event.target.value);
                }}
              />
            </div>
            <div className="field">
              <label htmlFor="package-profile">{messages.t('packages.profile')}</label>
              <select
                id="package-profile"
                value={profileID}
                aria-describedby={profilesError === null ? undefined : 'package-profile-error'}
                onChange={(event) => {
                  setProfileID(event.target.value);
                  apply({ q: query.trim(), profileID: event.target.value, offset: 0 });
                }}
              >
                <option value="">{messages.t('packages.all')}</option>
                {profiles.map((profile) => (
                  <option key={profile.profile_id} value={profile.profile_id}>
                    {profile.default
                      ? `${profile.profile_id} · ${messages.t('packages.default')}`
                      : profile.profile_id}
                  </option>
                ))}
              </select>
              {profilesError === null ? null : (
                <p className="hint field-error" id="package-profile-error">
                  {messages.t('common.loadfail')}
                  {profilesError}
                </p>
              )}
            </div>
            <button className="btn blue" type="submit">
              {messages.t('packages.search')}
            </button>
          </form>
        </div>
      </div>

      <div className="card packages-page">
        {/* State text only: the count, and nothing that changes on a poll. */}
        <p className="package-summary" role="status">
          {messages.plural('packages.count', total, { count: messages.number(total) })}
        </p>
        {/* The reserve is on this wrapper and never on tbody: a tbody with a
            min-height is a table row box with a height nothing in the table
            model honours, and the columns still have to be pinned before the
            first payload decides how wide they are.

            The state box is inside the reserve rather than under it, which is
            the half that is easy to miss: a loading sentence in a block of its
            own is 90px that collapses to nothing the moment rows arrive, so a
            reserve that only covered the table still moved the pager up by the
            height of the sentence it had just removed. */}
        <div className="packages-list">
          <div
            className="table-scroll"
            tabIndex={0}
            role="region"
            aria-label={messages.t('packages.h1')}
            aria-busy={pending ? 'true' : undefined}
          >
            <table className="list packages" data-cols="7">
              <thead>
                <tr>
                  <th>{messages.t('th.package')}</th>
                  <th>{messages.t('th.version')}</th>
                  <th>{messages.t('packages.profile')}</th>
                  <th>{messages.t('packages.sync')}</th>
                  <th>{messages.t('th.arch')}</th>
                  <th>{messages.t('packages.signature')}</th>
                  <th>{messages.t('packages.download')}</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((pkg) => (
                  <PackageRow
                    // Neither field is unique alone — one profile publishes many
                    // artifacts, and the same download path appears under two
                    // profiles — so the key is the pair, joined by a separator
                    // neither can contain. Spelled as an escape and never as the
                    // byte itself: a literal NUL makes the file non-text, and
                    // `grep`, `file` and every CI sweep built on them then skip
                    // it in silence instead of reporting it.
                    key={`${pkg.profile_id}\u0000${pkg.download_path}`}
                    pkg={pkg}
                    syncPath={syncPaths.get(pkg.profile_id) ?? pkg.binhost_path}
                  />
                ))}
              </tbody>
            </table>
          </div>
          <StateBox
            state={resolved}
            emptyKey="packages.empty"
            filteredKey="packages.none"
            errorKey="packages.loadfail"
            onRetry={() => {
              setAttempt((value) => value + 1);
            }}
          />
        </div>
        <div className="public-pagination">
          <button
            className="btn"
            type="button"
            aria-disabled={atStart ? 'true' : 'false'}
            onClick={() => {
              if (atStart) {
                return;
              }
              setFilter({ ...filter, offset: Math.max(0, filter.offset - PAGE_SIZE) });
            }}
          >
            {messages.t('packages.prev')}
          </button>
          {/* tabular figures and a reserved width, because the digit COUNT is
              what changes here — 9–58 becoming 59–108 moved both buttons. */}
          <span className="page-range">
            {messages.t('packages.page', {
              start: messages.number(start),
              end: messages.number(end),
            })}
          </span>
          <button
            className="btn"
            type="button"
            aria-disabled={atEnd ? 'true' : 'false'}
            onClick={() => {
              if (atEnd) {
                return;
              }
              setFilter({ ...filter, offset: filter.offset + PAGE_SIZE });
            }}
          >
            {messages.t('packages.next')}
          </button>
        </div>
      </div>

      {/* The catalogue's own caveat, kept next to the signature column it
          qualifies: "published" is a statement about this repository and not a
          substitute for the client-side verification /docs sets up. */}
      <p className="packages-verify-note">{messages.t('docs.verify.note')}</p>
    </>
  );
}

/**
 * Whether the catalogue reports a signature for this artifact.
 *
 * `null` is "the payload does not carry the field", which is every row today,
 * and it is deliberately not the same answer as `false`. Inferring "signed"
 * from the deployment holding a signing key is precisely the inference a
 * catalogue page must not make: the reader would be told an artifact is signed
 * by a page that has never seen a signature.
 */
function signatureToken(pkg: PublicPackage): string | null {
  if (pkg.signed === undefined) {
    return null;
  }
  return pkg.signed ? 'signed' : 'unsigned';
}

function PackageRow({ pkg, syncPath }: { pkg: PublicPackage; syncPath: string }) {
  const messages = useMessages();
  const flags = pkg.use_flags ?? [];
  return (
    <tr>
      <td>
        <span className="package-name">{pkg.name}</span>
        {/* Rendered whole rather than cut at 72 characters with the rest in a
            `title`. A truncated USE list is the same defect as the hidden sync
            path: the part that decides whether this artifact is the one you
            want is the part that was hidden. */}
        {flags.length > 0 ? <span className="package-flags">{flags.join(' ')}</span> : null}
      </td>
      <td className="mono">{pkg.version}</td>
      <td className="package-profile">{pkg.profile_id}</td>
      <td className="package-sync mono">{syncPath}</td>
      <td>{pkg.arch}</td>
      <td>
        <StatusBadge
          token={signatureToken(pkg)}
          absentLabel={messages.t('packages.signature.unreported')}
        />
      </td>
      <td>
        <a href={pkg.download_path}>{messages.t('packages.download')}</a>
      </td>
    </tr>
  );
}
