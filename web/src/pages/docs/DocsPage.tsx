/**
 * The anonymous documentation page.
 *
 * The prose is the same prose; the three commands are not. In the console this
 * replaces they were literals — `https://SERVER`, the profile id
 * `pe/amd64/glibc/systemd/base-v1`, and a binhost path ending in `TARGET` — and
 * none of the three describes this deployment. A reader who did what the page
 * says would write a binrepos.conf pointing at a host that does not exist, for
 * a profile that is not published here. That is worse than no example: it fails
 * at `emerge` time, several steps away from the page that caused it.
 *
 * So the commands are templated from the two things the console already knows:
 * the host the reader is already talking to, and the binhost inventory that
 * names every published profile with the exact `sync_path` the server serves it
 * at. Nothing here reassembles a path out of parts — `sync-uri` is the origin
 * plus the server's own `sync_path`, and the profile id is the server's own id.
 *
 * The profile selector exists because /binpkgs is not an aggregate repository:
 * every profile is an independent PKGDIR, which the prose beside it says, and a
 * page that could only ever template the default profile would contradict it.
 */

import { useCallback, useEffect, useState } from 'react';
import { Link } from 'react-router';

import type { ApiOutcome } from '../../api/client';
import { api } from '../../api/endpoints';
import { resolveState } from '../../api/state';
import type { BinhostInventory, BinhostProfile } from '../../api/types';
import { useMessages } from '../../i18n/context';
import { StateBox } from '../anonymous/StateBox';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import '../../styles/pages/docs.css';

/**
 * The atom the examples emerge.
 *
 * Deliberately not read off the catalogue: an example command carrying whatever
 * happened to be published first reads as an instruction to install that
 * package. The host, the profile id and the binhost path are the three things
 * that have to be true of this deployment for a copied command to work, and
 * they are the three that are templated.
 */
const EXAMPLE_ATOM = 'app-misc/jq';

function chooseProfile(
  profiles: readonly BinhostProfile[],
  selected: string,
): BinhostProfile | null {
  const named = profiles.find((profile) => profile.profile_id === selected);
  if (named !== undefined) {
    return named;
  }
  return profiles.find((profile) => profile.default) ?? profiles[0] ?? null;
}

export function DocsPage() {
  const messages = useMessages();
  useDocumentTitle('title.docs');

  const [answer, setAnswer] = useState<{
    attempt: number;
    outcome: ApiOutcome<BinhostInventory>;
  } | null>(null);
  const [selected, setSelected] = useState('');
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    void api.publicBinhosts({ signal: controller.signal }).then((outcome) => {
      if (!controller.signal.aborted) {
        setAnswer({ attempt, outcome });
      }
    });
    return () => {
      controller.abort();
    };
  }, [attempt]);

  // Derived rather than reset from inside the effect: an answer to an earlier
  // attempt is not an answer to this one, and saying so here costs no extra
  // render pass and leaves one source for the fact.
  const outcome = answer !== null && answer.attempt === attempt ? answer.outcome : null;
  const profiles = outcome?.kind === 'ok' ? outcome.value.binhosts : [];
  const resolved = resolveState<BinhostInventory>({ outcome, count: profiles.length });
  const profile = chooseProfile(profiles, selected);
  const retry = useCallback(() => {
    setAttempt((value) => value + 1);
  }, []);

  // The host the reader is already talking to. Not configurable and not
  // fetched: a documentation page that tells you to point at a different host
  // than the one serving it is the defect this page had.
  const origin = window.location.origin;
  const profileID = profile?.profile_id ?? '';
  const syncURI = profile === null ? '' : origin + profile.sync_path;

  return (
    <>
      <div className="public-head">
        <h1>{messages.t('docs.h1')}</h1>
        <p>{messages.t('docs.sub')}</p>
      </div>
      <div className="card public-docs docs-page">
        <div className="card-pad docs-body">
          <h2>{messages.t('docs.consume')}</h2>
          <p>{messages.t('docs.consume.p')}</p>
          <p>
            <Link to="/packages">{messages.t('docs.browse')}</Link>
          </p>

          {/* The selector's own height is reserved here rather than by an empty
              disabled control standing in for it: a select with no options is a
              control a reader can reach and learn nothing from, and the state
              of the section is already carried by the live region below. */}
          <div className="docs-profile">
            {profiles.length === 0 ? null : (
              <div className="field">
                <label htmlFor="docs-profile">{messages.t('packages.profile')}</label>
                <select
                  id="docs-profile"
                  value={profileID}
                  onChange={(event) => {
                    setSelected(event.target.value);
                  }}
                >
                  {profiles.map((candidate) => (
                    <option key={candidate.profile_id} value={candidate.profile_id}>
                      {candidate.default
                        ? `${candidate.profile_id} · ${messages.t('packages.default')}`
                        : candidate.profile_id}
                    </option>
                  ))}
                </select>
              </div>
            )}
            {/* One live region for the whole templated section, carrying its
                state and nothing else. The three blocks below hold their height
                while it is unresolved, so nothing under them moves when the
                inventory lands. */}
            <StateBox
              state={resolved}
              emptyKey="packages.empty"
              filteredKey="packages.empty"
              errorKey="common.loadfail"
              onRetry={retry}
            />
          </div>

          <h2>{messages.t('docs.client')}</h2>
          <p>{messages.t('docs.client.p')}</p>
          <pre className="docs-command" data-lines="5">
            {profile === null
              ? ''
              : `sudo portage-client configure \\\n` +
                `  -server=${origin} \\\n` +
                `  -profile-id=${profileID}\n\n` +
                `emerge --getbinpkg ${EXAMPLE_ATOM}`}
          </pre>

          <h2>{messages.t('docs.manual')}</h2>
          <p>{messages.t('docs.manual.p')}</p>
          <pre className="docs-command" data-lines="4">
            {profile === null
              ? ''
              : `[portage-engine]\npriority = 1\nsync-uri = ${syncURI}\nverify-signature = true`}
          </pre>

          <h2>{messages.t('docs.gpg')}</h2>
          <p>{messages.t('docs.gpg.p')}</p>
          <p className="notice">{messages.t('docs.verify.note')}</p>

          <h2>{messages.t('docs.build')}</h2>
          <p>{messages.t('docs.build.p')}</p>
          <pre className="docs-command" data-lines="6">
            {profile === null
              ? ''
              : `portage-client build \\\n` +
                `  -server=${origin} \\\n` +
                `  -project=PROJECT \\\n` +
                `  -profile-id=${profileID} \\\n` +
                `  -package=${EXAMPLE_ATOM} \\\n` +
                `  -wait`}
          </pre>
          <p>{messages.t('docs.build.p2')}</p>
        </div>
      </div>
    </>
  );
}
