import { Fragment, useCallback, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';

import type { ApiOutcome } from '../../api/client';
import { api } from '../../api/endpoints';
import type { Scope } from '../../api/endpoints';
import { useProjectScope } from '../../app/project';
import type { PageProps } from '../../app/routes';
import { useStepUp } from '../../app/stepup';
import type { CloudSettings, CloudSettingsResponse } from '../../api/types';
import { busyAttributes, isCompositionEnter, singleWriter } from '../../api/write';
import type { SingleWriter } from '../../api/write';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import { useMessages } from '../../i18n/context';
import '../../styles/pages/settings.css';
import { GpgPanel } from './GpgPanel';
import { SecurityPanel } from './SecurityPanel';
import { TestBuildCard } from './TestBuildCard';
import { TestConnectionCard } from './TestConnectionCard';
import { failureMessage } from './api';
import { attributeFailure } from './attribution';
import type { FieldAttribution } from './attribution';
import type { FormBinding } from './fields';
import {
  EMPTY_DRAFT,
  UNKNOWN_SECRETS,
  clearSecrets,
  fromResponse,
  isSecretControl,
  secretsFromResponse,
  toPayload,
  withText,
} from './model';
import type { ControlID, SettingsDraft, TextControl } from './model';
import { IDLE, noticeText } from './notice';
import type { Notice } from './notice';
import {
  AWSPanel,
  BuildConfPanel,
  BuildersPanel,
  GCPPanel,
  GeneralPanel,
  MirrorsPanel,
  NetPanel,
  PVEPanel,
  Panel,
  SSHPanel,
} from './panels';
import { SECTIONS, isSectionID } from './sections';
import type { SectionID } from './sections';
import { decodeEntities } from './text';

/**
 * The settings page: eleven panels, forty-five controls, one writer.
 *
 * The whole surface exists to write one resource, `/api/settings/cloud`, and
 * the single most expensive defect in the console this replaces lived in how it
 * did that. Two clicks eighty milliseconds apart produced two complete writes —
 * the second was queued and replayed, not dropped — and a successful write ends
 * by emptying the four secret inputs, because an empty secret is the wire
 * spelling of "keep the stored one". So the replayed submission collected the
 * inputs the first write had just cleared and PUT blank Proxmox and AWS
 * credentials over good ones. One double click on a slow network silently wiped
 * the credentials every build depends on, and the form looked saved either way.
 *
 * Three things together make that impossible here, and none of them is a
 * disabled attribute:
 *
 *   - the in-flight flag belongs to the RESOURCE, not to a button, because Save
 *     and Start Test Build both write it (`singleWriter`, created once);
 *   - a second activation is answered with `null` and never awaited, so there
 *     is no queue to replay;
 *   - the draft is state, and the secret inputs are emptied by the same commit
 *     that fills the form from the server's answer — after the flight has
 *     ended, and only then.
 *
 * The button stays in the tab order throughout: `aria-disabled` and `aria-busy`,
 * never `disabled`, which would drop the control a keyboard reader just used.
 */
export function SettingsPage(props: Partial<PageProps> = {}) {
  const messages = useMessages();
  const { projectID, ready } = useProjectScope();
  // Three resources, three prompts, and all three rendered by this component.
  //
  // The settings resource's own — Save and Start Test Build both write through
  // `save`, so one prompt covers both — plus the two the panels below need:
  // Test Connection posts a different route, and the session panel revokes
  // credentials. They are separate because they are separate authorities and a
  // refusal on one must not present itself as a refusal on another.
  //
  // What they are NOT is rendered where they are used. `useStepUp` portals its
  // dialog to the body, which moves the ELEMENT; the node stays where it was
  // declared in the React tree, and React propagates a submit along that tree.
  // Declared inside a panel, an answered credential prompt therefore reached
  // `#settings-form`'s onSubmit and PUT all forty-five controls — and a
  // successful PUT empties the four secret inputs, where an empty secret is the
  // wire spelling of "keep the stored one". So the panels take a guard and the
  // page owns the prompts, below the form.
  //
  // The method comes off the payload's own field, not off the principal: the
  // server sends no principal for a federated session, so reading it there
  // returned nothing and every OIDC operator was offered the local password
  // prompt their session can never satisfy.
  const sessionMethod = props.boot?.step_up_method ?? 'unstated';
  const stepUp = useStepUp(sessionMethod);
  const connectionStepUp = useStepUp(sessionMethod);
  const sessionStepUp = useStepUp(sessionMethod);
  // Held in its own binding because it never changes identity, and the writer
  // below is built once and must not be rebuilt: a rebuilt writer comes back
  // with its in-flight flag clear.
  const guard = stepUp.guard;

  const [draft, setDraft] = useState<SettingsDraft>(EMPTY_DRAFT);
  const [secrets, setSecrets] = useState(UNKNOWN_SECRETS);
  const [section, setSection] = useState<SectionID>('general');
  const [invalid, setInvalid] = useState<FieldAttribution | null>(null);
  const [notice, setNotice] = useState<Notice>(IDLE);
  const [busy, setBusy] = useState(false);
  const controls = useRef(new Map<ControlID, HTMLElement>());
  const noticeRef = useRef<HTMLSpanElement>(null);
  /** What to move focus to once the commit that opened its panel has landed. */
  const focusTarget = useRef<ControlID | 'notice' | null>(null);

  /**
   * The body of the write, and the writer that sends it.
   *
   * The in-flight flag belongs to the resource and has to survive every render,
   * so the writer is built once and never rebuilt — which means its thunk
   * cannot close over `draft`, because that would be a new thunk on every
   * keystroke and a new writer with an empty flag. It reads this slot instead,
   * which `save` fills immediately before the flight starts and only when
   * nothing is in flight: there is no window in which one write could pick up
   * another's body, and nothing typed while a request is out can change what
   * that request already sent.
   */
  const outboundRef = useRef<Partial<CloudSettings>>({});
  /** The project the write is made in, filled beside the body for the same reason. */
  const outboundScope = useRef<Scope>({});
  /** Built on first use and never again; `save` is its only caller. */
  const writerRef = useRef<SingleWriter<ApiOutcome<CloudSettingsResponse>> | null>(null);

  const collect = useCallback((): Partial<CloudSettings> => toPayload(draft), [draft]);

  // The route's own row names the title, so the catalogue key lives in the
  // table with every other route's; the literal is the floor for a render that
  // was handed no row, which is what a test mounting this page alone does.
  useDocumentTitle(props.route?.titleKey ?? 'title.settings');

  /* ---- load ------------------------------------------------------------ */

  useEffect(() => {
    if (!ready) {
      return undefined;
    }
    const controller = new AbortController();
    void api.cloudSettings({ projectID, signal: controller.signal }).then((outcome) => {
      if (controller.signal.aborted) {
        return;
      }
      if (outcome.kind !== 'ok') {
        setNotice({ state: 'failed', key: 'set.loadfail', detail: failureMessage(outcome) });
        return;
      }
      setSecrets(secretsFromResponse(outcome.value));
      // Merged onto whatever is already typed rather than replacing it: the
      // response redacts every secret, so reading one out of it would blank an
      // input the operator is in the middle of using.
      setDraft((previous) => fromResponse(outcome.value, previous));
    });
    return () => {
      controller.abort();
    };
  }, [projectID, ready]);

  /* ---- which controls a rejection may be attributed to ------------------ */

  /**
   * A disabled control takes no focus and carries no `aria-invalid` a reader
   * will ever reach, so attributing to one is indistinguishable from
   * attributing to nothing. Two cases are real: the mandatory verification
   * checkbox, which the server refuses to let anyone turn off, and the four
   * secret inputs when the deployment owns the values.
   */
  const unreachable = useCallback(
    (control: ControlID): boolean =>
      control === 'verify_install' || (secrets.external && isSecretControl(control)),
    [secrets.external],
  );

  const attribute = useCallback(
    (message: string): boolean => {
      const found = attributeFailure(message, unreachable);
      setInvalid(found);
      if (found === null) {
        return false;
      }
      // The panel first, then the focus: the control is inside a `hidden`
      // section until this commit, and a hidden element cannot be focused and
      // its role="alert" would never be announced. So the target is recorded
      // and the effect below spends it once the commit has landed.
      setSection(found.section);
      focusTarget.current = found.control;
      return true;
    },
    [unreachable],
  );

  // Moving the reading position is a side effect on the document, so it belongs
  // in an effect and not in the handler that decided it: the panel it lands in
  // is hidden until this commit paints. The three values are what a rejection
  // changes, so the effect runs on exactly the commits that can have produced a
  // target and on none of the others.
  useEffect(() => {
    const target = focusTarget.current;
    if (target === null) {
      return;
    }
    focusTarget.current = null;
    if (target === 'notice') {
      noticeRef.current?.focus();
      return;
    }
    controls.current.get(target)?.focus();
  }, [invalid, section, notice]);

  /* ---- the one writer -------------------------------------------------- */

  const save = useCallback(
    async (onStart?: () => void): Promise<ApiOutcome<CloudSettingsResponse> | null> => {
      const writer = (writerRef.current ??= singleWriter<ApiOutcome<CloudSettingsResponse>>(() =>
        // Through the step-up guard, so a control plane that wants a fresh
        // credential gets one and the PUT is repeated with it — inside the
        // writer, so the second attempt is still the one write this flag
        // guards and cannot race a second activation.
        guard(() => api.saveCloudSettings(outboundRef.current, outboundScope.current)),
      ));
      // Discarded, not queued, and answered before any state is touched: a second
      // activation that still wrote to the live region would announce one
      // operation twice. `onStart` is what lets a caller put "Saving…" on screen
      // for a write that is actually going out, and for no other.
      if (writer.pending()) {
        return null;
      }
      outboundRef.current = toPayload(draft);
      outboundScope.current = { projectID };
      onStart?.();
      setBusy(true);
      try {
        const outcome = await writer.run();
        if (outcome === null) {
          return null;
        }
        if (outcome.kind === 'ok') {
          const saved = outcome.value;
          setSecrets(secretsFromResponse(saved));
          setDraft((previous) => {
            const next = fromResponse(saved, previous);
            // All four secrets, and only once nothing is in flight. Cleared any
            // earlier, the very next collect() would send blanks for credentials
            // the operator typed seconds ago — and the server reads a blank as
            // "keep the stored one", so nothing on screen would say which
            // happened. The console this replaces cleared three of the four here
            // and forgot the mirror password entirely.
            return writer.pending() ? next : clearSecrets(next);
          });
        }
        return outcome;
      } finally {
        setBusy(false);
      }
    },
    [draft, guard, projectID],
  );

  /**
   * A rejection the reader cannot see is a rejection they cannot act on.
   *
   * The server answers a bad PUT with prose and no structure, so when that
   * prose names a control the panel holding it opens and focus moves there.
   * When it names none — or names one the reader cannot reach — the footer is
   * the only place the failure exists, so focus moves to the footer instead: a
   * form that stays put reads as a form that did nothing at all.
   */
  const reportFailure = useCallback(
    (detail: string) => {
      setNotice({ state: 'failed', key: 'set.savefail', detail });
      if (!attribute(detail)) {
        focusTarget.current = 'notice';
      }
    },
    [attribute],
  );

  const submit = useCallback(async () => {
    const outcome = await save(() => {
      setInvalid(null);
      setNotice({ state: 'busy', key: 'set.saving' });
    });
    // Discarded. The live region keeps whatever the write in flight put there:
    // one operation, announced once.
    if (outcome === null) {
      return;
    }
    if (outcome.kind === 'ok') {
      setNotice({ state: 'ok', key: 'set.saved' });
      return;
    }
    reportFailure(failureMessage(outcome));
  }, [reportFailure, save]);

  /* ---- sub-navigation --------------------------------------------------- */

  // The links carry href="#<section>" so they are in the tab order and operable
  // with Enter without any script, and each panel carries the same string as
  // its id, so a deep link resolves to a real element the browser can move
  // reading position to. Which panel is showing lives on the elements as
  // aria-current and hidden, never as a styling class — the stylesheet reads
  // the same attribute a screen reader does.
  useEffect(() => {
    const apply = (): void => {
      const fragment = location.hash.slice(1);
      if (isSectionID(fragment)) {
        setSection(fragment);
      }
    };
    apply();
    window.addEventListener('hashchange', apply);
    return () => {
      window.removeEventListener('hashchange', apply);
    };
  }, []);

  /* ---- the binding every field reads ------------------------------------ */

  const form: FormBinding = {
    draft,
    secrets,
    invalid,
    setText: (control: TextControl, value: string) => {
      setDraft((previous) => withText(previous, control, value));
    },
    setFlag: (control, value) => {
      setDraft((previous) =>
        control === 'pve_insecure'
          ? { ...previous, pveInsecure: value }
          : { ...previous, sshInsecure: value },
      );
    },
    setPlacement: (value) => {
      setDraft((previous) => ({ ...previous, placement: value }));
    },
    register: (control, node) => {
      if (node === null) {
        controls.current.delete(control);
      } else {
        controls.current.set(control, node);
      }
    },
  };

  const panelBody: Record<SectionID, ReactNode> = {
    general: (
      <GeneralPanel
        form={form}
        testBuild={<TestBuildCard form={form} save={save} attribute={attribute} />}
      />
    ),
    pve: (
      <PVEPanel
        form={form}
        connectionTest={<TestConnectionCard collect={collect} guard={connectionStepUp.guard} />}
      />
    ),
    gcp: <GCPPanel form={form} />,
    aws: <AWSPanel form={form} />,
    builders: <BuildersPanel form={form} />,
    mirrors: <MirrorsPanel form={form} />,
    buildconf: <BuildConfPanel form={form} />,
    ssh: <SSHPanel form={form} />,
    gpg: <GpgPanel />,
    net: <NetPanel form={form} />,
    security: <SecurityPanel guard={sessionStepUp.guard} />,
  };

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{messages.t('set.h1')}</h1>
          <p className="sub">{messages.t('set.sub')}</p>
        </div>
      </div>
      <div className="settings-layout">
        <nav className="subnav" id="subnav" aria-label={messages.t('set.h1')}>
          {SECTIONS.map((entry) => (
            // A fragment, so the category heading and its first link are two
            // siblings of the rail exactly as the Go markup has them — a
            // wrapper element here would become the flex item and the rail
            // would lay out eleven boxes instead of fourteen children.
            <Fragment key={entry.id}>
              {entry.groupKey === undefined ? null : (
                <span className="subnav-label">{messages.t(entry.groupKey)}</span>
              )}
              <a
                href={`#${entry.id}`}
                data-sec={entry.id}
                aria-current={section === entry.id ? 'true' : undefined}
                onClick={() => {
                  setSection(entry.id);
                }}
              >
                {decodeEntities(messages.t(entry.labelKey))}
              </a>
            </Fragment>
          ))}
        </nav>
        <div className="settings-panels">
          <form
            id="settings-form"
            onKeyDown={(event) => {
              // An Enter that only confirmed an IME candidate is not a
              // submission. Both shipped languages are typed through an IME at
              // least some of the time, and it is checked at the form so no
              // individual control has to remember.
              if (
                isCompositionEnter({ key: event.key, isComposing: event.nativeEvent.isComposing })
              ) {
                event.preventDefault();
              }
            }}
            onSubmit={(event) => {
              event.preventDefault();
              void submit();
            }}
          >
            {SECTIONS.map((entry) => (
              <Panel key={entry.id} id={entry.id} hidden={section !== entry.id}>
                {panelBody[entry.id]}
              </Panel>
            ))}
            <div className="settings-footer">
              <button className="btn blue" type="submit" {...busyAttributes(busy)}>
                {messages.t('set.save')}
              </button>
              {/* tabindex="-1" adds no tab stop; it is what lets a rejection
                  nobody could attribute to a field still receive focus, so the
                  reader is put on the sentence that says the save failed
                  instead of on an unchanged form. The node carries the sentence
                  and nothing else — a live region stamped on a box that also
                  holds rendered content re-announces that content every time
                  anything in it changes. */}
              <span
                className="save-msg settings-msg"
                data-state={notice.state}
                role="status"
                tabIndex={-1}
                ref={noticeRef}
              >
                {noticeText(messages, notice)}
              </span>
            </div>
          </form>
        </div>
      </div>
      {/* Over the form rather than instead of it: forty-five controls the
          operator has just filled in are not something to throw away in order
          to ask for a password. Outside it, too — a sibling of the form and
          never a descendant — because that is what keeps answering one of these
          from submitting the other thing on this page. */}
      {stepUp.prompt}
      {connectionStepUp.prompt}
      {sessionStepUp.prompt}
    </>
  );
}
