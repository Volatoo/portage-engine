/**
 * The eleven panels, and which controls each one holds.
 *
 * The control lists are not documentation. They are how a server rejection
 * finds the panel it has to open: the console this replaces walked the DOM with
 * `control.closest('.panel')`, which only works while the offending control is
 * rendered, and every panel but one is hidden. Declaring the membership makes
 * "open the panel that holds `pve_endpoint`" a lookup that holds whether or not
 * anything is on screen, and lets a test assert that all forty-seven controls
 * live in exactly one section — the count the accessibility sweep measured.
 *
 * Group headings are the three the Go subnav prints: General, Infrastructure,
 * Access. They are `<span>`s, not links, and carry no tab stop.
 */

import type { MessageKey } from '../../i18n/messages';
import type { ControlID } from './model';

export type SectionID =
  | 'general'
  | 'pve'
  | 'gcp'
  | 'aws'
  | 'builders'
  | 'mirrors'
  | 'buildconf'
  | 'ssh'
  | 'gpg'
  | 'net'
  | 'security';

export interface SettingsSection {
  readonly id: SectionID;
  readonly labelKey: MessageKey;
  /** Printed above this item when it opens a new category. */
  readonly groupKey?: MessageKey;
  readonly controls: readonly ControlID[];
}

export const SECTIONS: readonly SettingsSection[] = [
  {
    id: 'general',
    labelKey: 'set.sec.general',
    groupKey: 'set.cat.general',
    controls: ['provider', 'ttl', 'verify_install', 'test_pkg'],
  },
  {
    id: 'pve',
    labelKey: 'set.pve',
    groupKey: 'set.cat.infra',
    controls: [
      'pve_endpoint',
      'pve_token_id',
      'pve_token_secret',
      'pve_username',
      'pve_password',
      'pve_insecure',
      'place_auto',
      'place_manual',
      'pve_node_manual',
      'pve_nodes',
      'pve_template',
      'pve_storage',
      'pve_network',
      'pve_nameserver',
      'pve_ip_config',
      'pve_gateway',
      'pve_cicustom',
    ],
  },
  {
    id: 'gcp',
    labelKey: 'set.gcp',
    controls: ['gcp_project', 'gcp_region', 'gcp_zone', 'gcp_key_file'],
  },
  {
    id: 'aws',
    labelKey: 'set.aws',
    controls: ['aws_region', 'aws_zone', 'aws_access_key', 'aws_secret_key'],
  },
  { id: 'builders', labelKey: 'set.sec.builders', controls: ['remote_builders'] },
  {
    id: 'mirrors',
    labelKey: 'set.sec.mirrors',
    controls: [
      'gentoo_mirror',
      'portage_sync_method',
      'portage_sync_uri',
      'upload_url',
      'upload_dir',
      'upload_user',
      'upload_password',
    ],
  },
  {
    id: 'buildconf',
    labelKey: 'set.sec.buildconf',
    controls: ['make_conf_extra', 'build_features'],
  },
  {
    id: 'ssh',
    labelKey: 'set.sec.ssh',
    groupKey: 'set.cat.access',
    controls: ['ssh_key_path', 'ssh_user', 'ssh_known_hosts', 'ssh_insecure'],
  },
  { id: 'gpg', labelKey: 'set.sec.gpg', controls: [] },
  {
    id: 'net',
    labelKey: 'set.sec.net',
    controls: ['callback', 'bin_path', 'bin_url', 'bin_sha256'],
  },
  { id: 'security', labelKey: 'set.sec.security', controls: [] },
];

/** Which panel holds a control. Built once from the table above. */
export const SECTION_OF_CONTROL: ReadonlyMap<ControlID, SectionID> = new Map(
  SECTIONS.flatMap((section) =>
    section.controls.map((control): [ControlID, SectionID] => [control, section.id]),
  ),
);

export function isSectionID(value: string): value is SectionID {
  return SECTIONS.some((section) => section.id === value);
}
