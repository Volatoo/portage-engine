/**
 * Turning a server rejection into somewhere to look.
 *
 * `/api/settings/cloud` answers a bad PUT with `http.Error` — a bare
 * `text/plain` sentence naming the JSON field it refused, and nothing
 * structured. So the only bridge from that sentence back to a control is the
 * wire name inside it, and this is the table that spells the bridge. Without
 * it, a rejection landed on the form footer while the offending input sat on
 * one of the ten panels the reader could not see.
 *
 * Two things here are deliberately not what the console this replaces did.
 *
 * The longest wire name wins. `pve_node` and `pve_nodes` are both real fields
 * and the shorter one is a prefix of the longer, so a first-match-wins scan in
 * declaration order attributed "pve_nodes must be…" to the single-node input
 * and focused the wrong control — a rejection pointing at a field the operator
 * never touched is worse than one pointing nowhere.
 *
 * And a control the reader cannot reach is not an attribution. A disabled input
 * takes no focus and carries no `aria-invalid` anybody will ever read, so
 * attributing to it is indistinguishable from attributing to nothing. Three
 * cases are real: the mandatory verification checkbox, which the server refuses
 * to let anyone turn off, and the four secret inputs when the deployment owns
 * the values. Those fall through, and the caller has to say the save failed at
 * form level instead.
 */

import type { ControlID } from './model';
import type { SectionID } from './sections';
import { SECTION_OF_CONTROL } from './sections';

/**
 * The wire names `toPayload` sends, mapped to the control that produced each.
 *
 * `build_mode` points at the FEATURES input because that is the control sitting
 * in the same card as the fixed build mode: the mode itself has no control to
 * point at, and the card is what the operator has to read.
 */
export const FIELD_CONTROLS: Readonly<Record<string, ControlID>> = {
  provider: 'provider',
  instance_ttl_minutes: 'ttl',
  skip_verify_install: 'verify_install',
  remote_builders: 'remote_builders',
  build_mode: 'build_features',
  gcp_project: 'gcp_project',
  gcp_region: 'gcp_region',
  gcp_zone: 'gcp_zone',
  gcp_key_file: 'gcp_key_file',
  aws_region: 'aws_region',
  aws_zone: 'aws_zone',
  aws_access_key: 'aws_access_key',
  aws_secret_key: 'aws_secret_key',
  pve_endpoint: 'pve_endpoint',
  pve_node: 'pve_node_manual',
  pve_nodes: 'pve_nodes',
  pve_token_id: 'pve_token_id',
  pve_token_secret: 'pve_token_secret',
  pve_username: 'pve_username',
  pve_password: 'pve_password',
  pve_storage: 'pve_storage',
  pve_network: 'pve_network',
  pve_template: 'pve_template',
  pve_cicustom: 'pve_cicustom',
  pve_nameserver: 'pve_nameserver',
  gentoo_mirror: 'gentoo_mirror',
  portage_sync_uri: 'portage_sync_uri',
  portage_sync_method: 'portage_sync_method',
  make_conf_extra: 'make_conf_extra',
  build_features: 'build_features',
  ssh_key_path: 'ssh_key_path',
  ssh_user: 'ssh_user',
  ssh_known_hosts: 'ssh_known_hosts',
  upload_url: 'upload_url',
  upload_user: 'upload_user',
  upload_password: 'upload_password',
  upload_dir: 'upload_dir',
  server_callback_url: 'callback',
  builder_binary_path: 'bin_path',
  builder_binary_url: 'bin_url',
  builder_binary_sha256: 'bin_sha256',
};

/**
 * Whether a sentence is naming a field, rather than happening to contain its
 * letters.
 *
 * Forty of the forty-one wire names carry an underscore, and no English
 * sentence contains `pve_endpoint` or `builder_binary_sha256` by accident, so a
 * word-boundary match is enough for those — and the boundary is what keeps
 * `pve_node` out of a rejection about `pve_nodes`, since `_` and the trailing
 * `s` are both word characters.
 *
 * `provider` is the exception and the reason this is not a bare `includes`. It
 * is the one name that is also an ordinary English word, and this very handler
 * uses it as one: the refusal for a deployment that injects its own credentials
 * ends "…via the deployment secret provider". Matched bare, that sentence
 * opened the Backend panel and focused the provider select — a rejection
 * pointing at a control the operator never touched, which is worse than one
 * pointing nowhere. So an underscore-free name is only taken when the server
 * quoted a value after it, which is the shape `fmt.Sprintf("unsupported
 * provider %q", …)` produces and the shape prose never does.
 */
const AMBIGUOUS = /^[a-z0-9]+$/;

const MENTIONS: ReadonlyMap<string, RegExp> = new Map(
  Object.keys(FIELD_CONTROLS).map((wire) => [
    wire,
    AMBIGUOUS.test(wire) ? new RegExp(`\\b${wire}\\b\\s*["'\`]`) : new RegExp(`\\b${wire}\\b`),
  ]),
);

/** Longest first, so a prefix can never take a longer name's rejection. */
const WIRE_NAMES: readonly string[] = Object.keys(FIELD_CONTROLS).sort(
  (left, right) => right.length - left.length,
);

export interface FieldAttribution {
  /** The JSON field the server named. */
  readonly wire: string;
  readonly control: ControlID;
  /** The panel to open before focus can land on the control. */
  readonly section: SectionID;
  /** The server's own sentence, kept whole. */
  readonly message: string;
}

/**
 * Which control a rejection belongs to, or null when none of them can carry it.
 *
 * `unreachable` answers whether a control is disabled right now. It is passed
 * in rather than read off the document because the answer depends on state the
 * page owns — whether the deployment injects the credentials — and because this
 * has to be decidable in a test with no DOM.
 */
export function attributeFailure(
  message: string,
  unreachable: (control: ControlID) => boolean,
): FieldAttribution | null {
  for (const wire of WIRE_NAMES) {
    if (MENTIONS.get(wire)?.test(message) !== true) {
      continue;
    }
    const control = FIELD_CONTROLS[wire];
    if (control === undefined || unreachable(control)) {
      continue;
    }
    const section = SECTION_OF_CONTROL.get(control);
    if (section === undefined) {
      continue;
    }
    return { wire, control, section, message };
  }
  return null;
}
