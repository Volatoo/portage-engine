/**
 * The settings form's model: what a control holds, and what goes on the wire.
 *
 * The form is one draft object rather than forty-five inputs read back out of
 * the document. That is not a React preference — it is what makes the write
 * safe. `collect()` in the console this replaces read the live DOM at the
 * moment of submission, so anything that emptied an input between two
 * submissions changed what the second one sent; the secret inputs are emptied
 * by a successful save, and that is precisely how a replayed submission came to
 * PUT blank Proxmox and AWS credentials over good ones. A draft that is state
 * cannot be mutated by a render.
 *
 * Control names are the ids the Go page uses (`pve_token_secret`,
 * `bin_sha256`), not re-spelled camelCase. One vocabulary runs from the Go
 * template through the wire-name table in ./attribution.ts to the tests, so a
 * rejection naming `pve_endpoint` can be traced to a control without a
 * translation step nobody maintains.
 */

import type { CloudSettings, CloudSettingsResponse } from '../../api/types';

/**
 * Every text, number, select and textarea control on the form.
 *
 * `test_pkg` is on this list and absent from the payload below: it is the test
 * build's argument, not a setting, and the Go page never sent it either.
 */
export const TEXT_CONTROLS = [
  'provider',
  'ttl',
  'test_pkg',
  'pve_endpoint',
  'pve_token_id',
  'pve_token_secret',
  'pve_username',
  'pve_password',
  'pve_node_manual',
  'pve_nodes',
  'pve_template',
  'pve_storage',
  'pve_network',
  'pve_nameserver',
  'pve_cicustom',
  'gcp_project',
  'gcp_region',
  'gcp_zone',
  'gcp_key_file',
  'aws_region',
  'aws_zone',
  'aws_access_key',
  'aws_secret_key',
  'remote_builders',
  'gentoo_mirror',
  'portage_sync_method',
  'portage_sync_uri',
  'upload_url',
  'upload_dir',
  'upload_user',
  'upload_password',
  'make_conf_extra',
  'build_features',
  'ssh_key_path',
  'ssh_user',
  'ssh_known_hosts',
  'callback',
  'bin_path',
  'bin_url',
  'bin_sha256',
] as const;

export type TextControl = (typeof TEXT_CONTROLS)[number];

/** The checkboxes. `verify_install` is stated, never chosen — see below. */
export type CheckControl = 'pve_insecure' | 'ssh_insecure' | 'verify_install';

/** Anything a server rejection can be attributed to. */
export type ControlID = TextControl | CheckControl | 'place_auto' | 'place_manual';

export interface SettingsDraft {
  readonly text: Readonly<Record<TextControl, string>>;
  readonly pveInsecure: boolean;
  readonly sshInsecure: boolean;
  /** The placement radio pair. 'auto' sends `pve_node: "auto"`. */
  readonly placement: 'auto' | 'manual';
}

/**
 * The four secrets the form declares.
 *
 * All four, in one list, used by both the "clear after a successful write" step
 * and the "is this control reachable" test in ./attribution.ts. The console
 * this replaces cleared three of them by hand at the call site and omitted
 * `upload_password`, so the mirror password behaved differently from the other
 * three for no reason anybody could state.
 */
export const SECRET_CONTROLS = [
  'pve_token_secret',
  'pve_password',
  'aws_secret_key',
  'upload_password',
] as const;

export type SecretControl = (typeof SECRET_CONTROLS)[number];

/** Which `has_*` boolean on the GET response says a secret is already stored. */
export const SECRET_PRESENCE: Readonly<Record<SecretControl, keyof CloudSettingsResponse>> = {
  pve_token_secret: 'has_pve_token_secret',
  pve_password: 'has_pve_password',
  aws_secret_key: 'has_aws_secret_key',
  upload_password: 'has_upload_password',
};

export function isSecretControl(control: string): control is SecretControl {
  return (SECRET_CONTROLS as readonly string[]).includes(control);
}

/** Whether a stored secret exists, per control, plus who owns the values. */
export interface SecretState {
  readonly present: Readonly<Record<SecretControl, boolean>>;
  /**
   * True when the deployment injects credentials and the settings API refuses
   * to carry them. The four inputs are then disabled and say so, which is also
   * why a rejection naming one of them cannot be attributed to it.
   */
  readonly external: boolean;
}

export const UNKNOWN_SECRETS: SecretState = {
  present: {
    pve_token_secret: false,
    pve_password: false,
    aws_secret_key: false,
    upload_password: false,
  },
  external: false,
};

function emptyText(): Record<TextControl, string> {
  const text = {} as Record<TextControl, string>;
  for (const control of TEXT_CONTROLS) {
    text[control] = '';
  }
  return text;
}

/**
 * What the form holds before the first GET answers.
 *
 * The three defaults are the ones the Go markup carried as literal attribute
 * values, so a form rendered before its payload arrives is the same form.
 */
export const EMPTY_DRAFT: SettingsDraft = {
  text: { ...emptyText(), provider: 'pve', ttl: '0', test_pkg: 'app-misc/jq' },
  pveInsecure: false,
  sshInsecure: false,
  placement: 'auto',
};

export function withText(draft: SettingsDraft, control: TextControl, value: string): SettingsDraft {
  return { ...draft, text: { ...draft.text, [control]: value } };
}

/**
 * Empty the four secret inputs.
 *
 * Only ever called once a write has finished and no other write is pending. An
 * empty secret is the wire spelling of "keep the stored one", so a submission
 * that collects a cleared input asks the server to keep a credential — which is
 * correct only if the write that cleared it actually stored the typed value
 * first.
 */
export function clearSecrets(draft: SettingsDraft): SettingsDraft {
  const text = { ...draft.text };
  for (const control of SECRET_CONTROLS) {
    text[control] = '';
  }
  return { ...draft, text };
}

function csvValue(list: readonly string[] | undefined): string {
  return (list ?? []).join(',');
}

function csvField(value: string): string[] {
  const trimmed = value.trim();
  if (trimmed === '') {
    return [];
  }
  return trimmed
    .split(',')
    .map((entry) => entry.trim())
    .filter((entry) => entry !== '');
}

/**
 * The GET response, as controls.
 *
 * `previous` carries the two things the payload has no opinion about: the test
 * build's package atom, and whatever the operator has typed into a secret input
 * and not yet saved. The response never returns a secret — it is redacted
 * server-side — so reading one out of it would blank the input on every reload.
 */
export function fromResponse(
  saved: CloudSettingsResponse,
  previous: SettingsDraft = EMPTY_DRAFT,
): SettingsDraft {
  // The server writes "auto" or a node name; an empty node is auto as well,
  // because a cluster with no pin is scheduled by load.
  const node = saved.pve_node ?? '';
  const auto = node === '' || node.toLowerCase() === 'auto';
  return {
    text: {
      provider: saved.provider || 'pve',
      ttl: String(saved.instance_ttl_minutes || 0),
      test_pkg: previous.text.test_pkg,
      pve_endpoint: saved.pve_endpoint ?? '',
      pve_token_id: saved.pve_token_id ?? '',
      pve_token_secret: previous.text.pve_token_secret,
      pve_username: saved.pve_username ?? '',
      pve_password: previous.text.pve_password,
      pve_node_manual: auto ? '' : node,
      pve_nodes: csvValue(saved.pve_nodes),
      pve_template: saved.pve_template ?? '',
      pve_storage: saved.pve_storage ?? '',
      pve_network: saved.pve_network ?? '',
      pve_nameserver: saved.pve_nameserver ?? '',
      pve_cicustom: saved.pve_cicustom ?? '',
      gcp_project: saved.gcp_project ?? '',
      gcp_region: saved.gcp_region ?? '',
      gcp_zone: saved.gcp_zone ?? '',
      gcp_key_file: saved.gcp_key_file ?? '',
      aws_region: saved.aws_region ?? '',
      aws_zone: saved.aws_zone ?? '',
      aws_access_key: saved.aws_access_key ?? '',
      aws_secret_key: previous.text.aws_secret_key,
      remote_builders: csvValue(saved.remote_builders),
      gentoo_mirror: saved.gentoo_mirror ?? '',
      portage_sync_method: saved.portage_sync_method || 'webrsync',
      portage_sync_uri: saved.portage_sync_uri ?? '',
      upload_url: saved.upload_url ?? '',
      upload_dir: saved.upload_dir ?? '',
      upload_user: saved.upload_user ?? '',
      upload_password: previous.text.upload_password,
      make_conf_extra: saved.make_conf_extra ?? '',
      build_features: saved.build_features ?? '',
      ssh_key_path: saved.ssh_key_path ?? '',
      ssh_user: saved.ssh_user ?? '',
      ssh_known_hosts: saved.ssh_known_hosts ?? '',
      callback: saved.server_callback_url ?? '',
      bin_path: saved.builder_binary_path ?? '',
      bin_url: saved.builder_binary_url ?? '',
      bin_sha256: saved.builder_binary_sha256 ?? '',
    },
    pveInsecure: saved.pve_insecure,
    sshInsecure: saved.ssh_insecure_host_key,
    placement: auto ? 'auto' : 'manual',
  };
}

export function secretsFromResponse(saved: CloudSettingsResponse): SecretState {
  return {
    present: {
      pve_token_secret: saved.has_pve_token_secret,
      pve_password: saved.has_pve_password,
      aws_secret_key: saved.has_aws_secret_key,
      upload_password: saved.has_upload_password,
    },
    external: saved.secret_values_managed_externally,
  };
}

/**
 * The draft, as the PUT body.
 *
 * Two fields are stated rather than collected, and both are stated because the
 * server no longer accepts anything else: `skip_verify_install` is false
 * because quarantined install verification is mandatory before publication, and
 * `build_mode` is native-gentoo because the Docker builders are gone. Their
 * controls exist on the form to say so, disabled.
 *
 * `make_conf_extra` is the one value not trimmed. It is appended verbatim to a
 * generated make.conf, and leading whitespace on a continuation line is part of
 * what the operator wrote.
 */
export function toPayload(draft: SettingsDraft): Partial<CloudSettings> {
  const value = (control: TextControl): string => draft.text[control].trim();
  const node = draft.placement === 'manual' ? value('pve_node_manual') || 'pve' : 'auto';
  return {
    provider: value('provider'),
    instance_ttl_minutes: Number.parseInt(value('ttl') || '0', 10) || 0,
    skip_verify_install: false,
    remote_builders: csvField(draft.text.remote_builders),
    gcp_project: value('gcp_project'),
    gcp_region: value('gcp_region'),
    gcp_zone: value('gcp_zone'),
    gcp_key_file: value('gcp_key_file'),
    aws_region: value('aws_region'),
    aws_zone: value('aws_zone'),
    aws_access_key: value('aws_access_key'),
    aws_secret_key: value('aws_secret_key'),
    pve_endpoint: value('pve_endpoint'),
    pve_node: node,
    pve_nodes: csvField(draft.text.pve_nodes),
    pve_token_id: value('pve_token_id'),
    pve_token_secret: value('pve_token_secret'),
    pve_username: value('pve_username'),
    pve_password: value('pve_password'),
    pve_insecure: draft.pveInsecure,
    pve_storage: value('pve_storage'),
    pve_network: value('pve_network'),
    pve_template: value('pve_template'),
    pve_cicustom: value('pve_cicustom'),
    pve_nameserver: value('pve_nameserver'),
    gentoo_mirror: value('gentoo_mirror'),
    portage_sync_uri: value('portage_sync_uri'),
    portage_sync_method: value('portage_sync_method') || 'webrsync',
    make_conf_extra: draft.text.make_conf_extra,
    build_features: value('build_features'),
    build_mode: 'native-gentoo',
    ssh_key_path: value('ssh_key_path'),
    ssh_user: value('ssh_user'),
    ssh_known_hosts: value('ssh_known_hosts'),
    ssh_insecure_host_key: draft.sshInsecure,
    upload_url: value('upload_url'),
    upload_user: value('upload_user'),
    upload_password: value('upload_password'),
    upload_dir: value('upload_dir'),
    server_callback_url: value('callback'),
    builder_binary_path: value('bin_path'),
    builder_binary_url: value('bin_url'),
    builder_binary_sha256: value('bin_sha256'),
  };
}
