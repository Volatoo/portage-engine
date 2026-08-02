import type { ReactNode } from 'react';

import { useMessages } from '../../i18n/context';
import type { MessageKey } from '../../i18n/messages';
import type { FieldAttribution } from './attribution';
import type { ControlID, SecretState, SettingsDraft, TextControl } from './model';
import { decodeEntities } from './text';

/**
 * The five control shapes the form is built from.
 *
 * Every one of them renders a `<label for>` bound to the control's id — there
 * is no branch that produces a control without one, which is what makes
 * "forty-five fields, none unlabelled" a property of the code rather than a
 * measurement somebody has to take again after every change.
 *
 * A hint gets an id and joins `aria-describedby`. The console this replaces
 * wrote the format string for twenty-odd fields into a paragraph no assistive
 * technology was ever pointed at, and then, on a rejection, replaced the whole
 * `aria-describedby` with the error's id — so acting on the rejection cost the
 * reader the format they needed to fix it. Both are named here, in that order.
 */

export interface FormBinding {
  readonly draft: SettingsDraft;
  readonly secrets: SecretState;
  /** The one control a server rejection was attributed to, if any. */
  readonly invalid: FieldAttribution | null;
  setText(control: TextControl, value: string): void;
  setFlag(control: 'pve_insecure' | 'ssh_insecure', value: boolean): void;
  setPlacement(value: 'auto' | 'manual'): void;
  /**
   * Hands the page a node to move focus to. Registration rather than
   * `document.getElementById` because the id space is the document's and the
   * form's is not: two forms in one test would otherwise focus each other's
   * inputs.
   */
  register(control: ControlID, node: HTMLElement | null): void;
}

function describedBy(...ids: (string | null)[]): string | undefined {
  const named = ids.filter((id): id is string => id !== null);
  return named.length > 0 ? named.join(' ') : undefined;
}

/**
 * The rejection note for one control.
 *
 * `role="alert"` and not a live region on the field wrapper: this node exists
 * only to carry the sentence, and it is inserted at the moment the rejection
 * arrives, so it is announced once rather than re-read whenever anything else
 * in the field changes.
 */
function FieldError({ form, control }: { form: FormBinding; control: ControlID }) {
  const messages = useMessages();
  if (form.invalid === null || form.invalid.control !== control) {
    return null;
  }
  return (
    <p className="hint field-error" id={`${control}-error`} role="alert">
      {messages.t('set.field.invalid') + form.invalid.message}
    </p>
  );
}

function errorID(form: FormBinding, control: ControlID): string | null {
  return form.invalid !== null && form.invalid.control === control ? `${control}-error` : null;
}

function invalidFlag(form: FormBinding, control: ControlID): 'true' | undefined {
  return form.invalid !== null && form.invalid.control === control ? 'true' : undefined;
}

/** A hint paragraph, addressable so the control can name it. */
export function Hint({ id, children }: { id?: string | undefined; children: ReactNode }) {
  return (
    <p className="hint" id={id}>
      {children}
    </p>
  );
}

export interface TextFieldProps {
  form: FormBinding;
  control: TextControl;
  labelKey?: MessageKey | undefined;
  /**
   * A label the catalogue does not carry. Exactly one field uses it — see
   * UNTRANSLATED in ./text.ts, which says which and why.
   */
  label?: string | undefined;
  /** The static explanation under the control. */
  hintKey?: MessageKey | undefined;
  /** A hint the page computes — the secret-status sentence, and nothing else. */
  hint?: ReactNode | undefined;
  placeholder?: string | undefined;
  type?: 'text' | 'password' | 'number' | undefined;
  /** Only `bin_sha256` sets these two, and the server enforces both again. */
  maxLength?: number | undefined;
  min?: number | undefined;
  spellCheck?: boolean | undefined;
  disabled?: boolean | undefined;
  /** Present on the secret inputs, where the placeholder states secret status. */
  placeholderText?: string | undefined;
  autoComplete?: string | undefined;
  /**
   * Present in the document, out of the tab order. Used by the one pair of
   * fields that are alternatives — see the placement card — for the same
   * reason the closed panels are hidden rather than unmounted: the payload and
   * the labelling sweep both want a form whose shape does not depend on which
   * radio is selected, and a control nobody can see must not take focus.
   */
  fieldHidden?: boolean | undefined;
}

/**
 * The label's text.
 *
 * The last resort is the control's own name, never the empty string: a control
 * whose catalogue key went missing should read as `bin_sha256` and stay
 * countable in a labelling sweep, rather than render an empty `<label>` that
 * satisfies every automated check and tells a reader nothing.
 */
function labelText(
  messages: ReturnType<typeof useMessages>,
  key: MessageKey | undefined,
  literal: string | undefined,
  control: ControlID,
): string {
  if (key !== undefined) {
    return decodeEntities(messages.t(key));
  }
  return literal ?? control;
}

export function TextField(props: TextFieldProps) {
  const messages = useMessages();
  const { form, control } = props;
  const hintID = props.hintKey !== undefined || props.hint !== undefined ? `${control}-hint` : null;
  return (
    <div className="field" hidden={props.fieldHidden}>
      <label htmlFor={control}>{labelText(messages, props.labelKey, props.label, control)}</label>
      <input
        id={control}
        type={props.type ?? 'text'}
        value={form.draft.text[control]}
        onChange={(event) => {
          form.setText(control, event.target.value);
        }}
        ref={(node) => {
          form.register(control, node);
        }}
        placeholder={props.placeholderText ?? props.placeholder}
        maxLength={props.maxLength}
        min={props.min}
        spellCheck={props.spellCheck}
        disabled={props.disabled}
        autoComplete={props.autoComplete}
        aria-invalid={invalidFlag(form, control)}
        aria-describedby={describedBy(hintID, errorID(form, control))}
      />
      {props.hint !== undefined ? <Hint id={hintID ?? undefined}>{props.hint}</Hint> : null}
      {props.hintKey !== undefined ? (
        <Hint id={hintID ?? undefined}>{decodeEntities(messages.t(props.hintKey))}</Hint>
      ) : null}
      <FieldError form={form} control={control} />
    </div>
  );
}

export function TextAreaField(props: {
  form: FormBinding;
  control: TextControl;
  labelKey: MessageKey;
  hintKey: MessageKey;
  placeholder?: string | undefined;
}) {
  const messages = useMessages();
  const { form, control } = props;
  const hintID = `${control}-hint`;
  return (
    <div className="field">
      <label htmlFor={control}>{decodeEntities(messages.t(props.labelKey))}</label>
      <textarea
        id={control}
        spellCheck={false}
        value={form.draft.text[control]}
        onChange={(event) => {
          form.setText(control, event.target.value);
        }}
        ref={(node) => {
          form.register(control, node);
        }}
        placeholder={props.placeholder}
        aria-invalid={invalidFlag(form, control)}
        aria-describedby={describedBy(hintID, errorID(form, control))}
      />
      <Hint id={hintID}>{decodeEntities(messages.t(props.hintKey))}</Hint>
      <FieldError form={form} control={control} />
    </div>
  );
}

export function SelectField(props: {
  form: FormBinding;
  control: TextControl;
  labelKey: MessageKey;
  hintKey?: MessageKey | undefined;
  options: readonly { value: string; label: string }[];
}) {
  const messages = useMessages();
  const { form, control } = props;
  const hintID = props.hintKey !== undefined ? `${control}-hint` : null;
  return (
    <div className="field">
      <label htmlFor={control}>{decodeEntities(messages.t(props.labelKey))}</label>
      <select
        id={control}
        value={form.draft.text[control]}
        onChange={(event) => {
          form.setText(control, event.target.value);
        }}
        ref={(node) => {
          form.register(control, node);
        }}
        aria-invalid={invalidFlag(form, control)}
        aria-describedby={describedBy(hintID, errorID(form, control))}
      >
        {props.options.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
      {props.hintKey !== undefined ? (
        <Hint id={hintID ?? undefined}>{decodeEntities(messages.t(props.hintKey))}</Hint>
      ) : null}
      <FieldError form={form} control={control} />
    </div>
  );
}

/**
 * A checkbox and the sentence that is its label.
 *
 * `verify_install` is the one that is stated rather than chosen: the server
 * rejects `skip_verify_install: true` outright, so the control is disabled and
 * the label is the statement. It is still a labelled control, and it is still
 * what a rejection naming that field would point at — except that a disabled
 * control cannot be pointed at, which is why ./attribution.ts refuses it and
 * the failure goes to the footer instead.
 */
export function CheckField(props: {
  form: FormBinding;
  control: 'pve_insecure' | 'ssh_insecure' | 'verify_install';
  labelKey: MessageKey;
  checked?: boolean | undefined;
  disabled?: boolean | undefined;
}) {
  const messages = useMessages();
  const { form, control } = props;
  const stated = props.disabled === true;
  const checked = stated
    ? (props.checked ?? true)
    : control === 'pve_insecure'
      ? form.draft.pveInsecure
      : form.draft.sshInsecure;
  return (
    <div className="field check">
      <input
        id={control}
        type="checkbox"
        checked={checked}
        disabled={props.disabled}
        onChange={(event) => {
          if (control !== 'verify_install') {
            form.setFlag(control, event.target.checked);
          }
        }}
        ref={(node) => {
          form.register(control, node);
        }}
        aria-invalid={invalidFlag(form, control)}
        aria-describedby={describedBy(errorID(form, control))}
      />
      <label htmlFor={control}>{decodeEntities(messages.t(props.labelKey))}</label>
      <FieldError form={form} control={control} />
    </div>
  );
}

/**
 * The placement pair.
 *
 * Two radios in one group with one name, each with its own label — not a
 * `<select>`, because which of the two is chosen changes which field below is
 * shown, and a reader has to be able to see both answers at once to understand
 * that.
 */
export function PlacementRadios({ form }: { form: FormBinding }) {
  const messages = useMessages();
  return (
    <div className="radio-row">
      <label htmlFor="place_auto">
        <input
          id="place_auto"
          type="radio"
          name="placement"
          checked={form.draft.placement === 'auto'}
          onChange={() => {
            form.setPlacement('auto');
          }}
          ref={(node) => {
            form.register('place_auto', node);
          }}
        />
        <span>{messages.t('set.place.auto')}</span>
      </label>
      <label htmlFor="place_manual">
        <input
          id="place_manual"
          type="radio"
          name="placement"
          checked={form.draft.placement === 'manual'}
          onChange={() => {
            form.setPlacement('manual');
          }}
          ref={(node) => {
            form.register('place_manual', node);
          }}
        />
        <span>{messages.t('set.place.manual')}</span>
      </label>
    </div>
  );
}
