import type { ReactNode } from 'react';

import { useMessages } from '../../i18n/context';
import type { Messages } from '../../i18n/messages';
import { CheckField, Hint, PlacementRadios, SelectField, TextAreaField, TextField } from './fields';
import type { FormBinding } from './fields';
import type { SecretControl } from './model';
import { UNTRANSLATED, decodeEntities } from './text';

/**
 * The nine panels that are nothing but form.
 *
 * They are rendered all at once and all but one carries `hidden`. That is a
 * decision, not an inheritance: a control that cannot be seen must not take
 * focus, and `[hidden]` — which base.css forces to `display: none` — takes the
 * other ten panels' controls out of the tab order and out of the accessibility
 * tree together, so a reader tabbing off the sub-navigation lands on the panel
 * they just opened rather than forty-six controls further down. What they are
 * NOT is unmounted: the form is one form, the draft is one object, and Save
 * sends every field whether or not its panel was ever opened. Rendering only
 * the open panel would have made the payload depend on where the reader had
 * clicked, and would have cost the sweep its forty-five labelled controls.
 */

/**
 * The sentence beside a secret input, and the placeholder inside it.
 *
 * This is the only place the form says whether a credential is already stored —
 * there is nothing else on screen that could tell a configured deployment from
 * an unconfigured one, because the value itself is redacted server-side and
 * never reaches this bundle. Two channels on purpose: the hint is permanent
 * prose bound to the input through `aria-describedby`, and the placeholder is
 * what a sighted reader sees inside the empty box they are deciding whether to
 * type into.
 */
function secretHint(messages: Messages, present: boolean, external: boolean): string {
  if (external) {
    return present
      ? messages.t('set.secret.external.set')
      : messages.t('set.secret.external.unset');
  }
  return present ? messages.t('set.secret.saved') : messages.t('set.secret.unset');
}

function secretPlaceholder(
  messages: Messages,
  present: boolean,
  external: boolean,
): string | undefined {
  if (external) {
    return messages.t('set.secret.external.placeholder');
  }
  return present ? messages.t('set.secret.ph') : undefined;
}

/** A secret input, wired to both statements at once. */
function SecretField(props: {
  form: FormBinding;
  control: SecretControl;
  labelKey: Parameters<Messages['t']>[0];
  autoComplete?: string | undefined;
}) {
  const messages = useMessages();
  const present = props.form.secrets.present[props.control];
  const external = props.form.secrets.external;
  return (
    <TextField
      form={props.form}
      control={props.control}
      labelKey={props.labelKey}
      type="password"
      autoComplete={props.autoComplete ?? 'off'}
      disabled={external}
      placeholderText={secretPlaceholder(messages, present, external)}
      hint={secretHint(messages, present, external)}
    />
  );
}

export function Panel(props: { id: string; hidden: boolean; children: ReactNode }) {
  return (
    <section className="panel" id={props.id} data-panel={props.id} hidden={props.hidden}>
      {props.children}
    </section>
  );
}

export function Card(props: { titleKey?: Parameters<Messages['t']>[0]; children: ReactNode }) {
  const messages = useMessages();
  return (
    <div className="card">
      {props.titleKey === undefined ? null : (
        <h3 className="card-title">{decodeEntities(messages.t(props.titleKey))}</h3>
      )}
      {props.children}
    </div>
  );
}

/* ---- general: backend, plus the test build card the page owns ---------- */

export function GeneralPanel({ form, testBuild }: { form: FormBinding; testBuild: ReactNode }) {
  const messages = useMessages();
  return (
    <>
      <Card titleKey="set.backend">
        <div className="card-pad form-grid">
          <SelectField
            form={form}
            control="provider"
            labelKey="set.provider"
            hintKey="set.provider.hint"
            options={[
              { value: 'pve', label: messages.t('set.pve') },
              { value: 'gcp', label: messages.t('set.gcp') },
              { value: 'aws', label: messages.t('set.aws') },
            ]}
          />
          <TextField
            form={form}
            control="ttl"
            labelKey="set.ttl"
            hintKey="set.ttl.hint"
            type="number"
            min={0}
          />
        </div>
        <div className="card-pad settings-check-pad">
          <CheckField form={form} control="verify_install" labelKey="set.verify" checked disabled />
        </div>
      </Card>
      {testBuild}
    </>
  );
}

/* ---- proxmox ------------------------------------------------------------ */

export function PVEPanel({
  form,
  connectionTest,
}: {
  form: FormBinding;
  connectionTest: ReactNode;
}) {
  const messages = useMessages();
  const manual = form.draft.placement === 'manual';
  return (
    <>
      <Card titleKey="set.conn">
        <div className="card-pad">
          <div className="form-grid">
            <TextField
              form={form}
              control="pve_endpoint"
              labelKey="set.endpoint"
              placeholder="https://pve.example.com:8006"
            />
            <TextField
              form={form}
              control="pve_token_id"
              labelKey="set.tokenid"
              placeholder="root@pam!terraform"
            />
            <SecretField form={form} control="pve_token_secret" labelKey="set.secret" />
            <TextField
              form={form}
              control="pve_username"
              labelKey="set.pveuser"
              hintKey="set.pveuser.hint"
              placeholder="terraform-prov@pve"
              autoComplete="off"
            />
            <SecretField form={form} control="pve_password" labelKey="set.pvepass" />
          </div>
          <CheckField form={form} control="pve_insecure" labelKey="set.tls" />
        </div>
        {connectionTest}
      </Card>

      <Card titleKey="set.placement">
        <div className="card-pad">
          <PlacementRadios form={form} />
          <Hint>{messages.t('set.place.auto.hint')}</Hint>
          <div className="form-grid">
            {/* A pinned node and a candidate list are alternatives, so only one
                of them is on screen — but both stay in the document, hidden,
                for the same reason the closed panels do: the form keeps one
                shape whichever radio is selected, and `hidden` is what keeps
                the unchosen one out of the tab order and out of the
                accessibility tree. */}
            <TextField
              form={form}
              control="pve_node_manual"
              labelKey="set.node"
              placeholder="pve"
              fieldHidden={!manual}
            />
            <TextField
              form={form}
              control="pve_nodes"
              labelKey="set.nodes"
              hintKey="set.nodes.hint"
              placeholder="pve1,pve2,pve3"
              fieldHidden={manual}
            />
          </div>
        </div>
      </Card>

      <Card titleKey="set.resources">
        <div className="card-pad form-grid">
          <TextField
            form={form}
            control="pve_template"
            labelKey="set.template"
            hintKey="set.template.hint"
            placeholder="gentoo-native-cloudinit-template"
          />
          <TextField
            form={form}
            control="pve_storage"
            labelKey="set.storage"
            placeholder="local-lvm"
          />
          <TextField form={form} control="pve_network" labelKey="set.bridge" placeholder="vmbr0" />
          <TextField
            form={form}
            control="pve_nameserver"
            labelKey="set.nameserver"
            hintKey="set.nameserver.hint"
            placeholder="10.0.0.252"
          />
          <TextField
            form={form}
            control="pve_cicustom"
            labelKey="set.cicustom"
            hintKey="set.cicustom.hint"
            placeholder="vendor=local:snippets/vendor.yaml"
          />
        </div>
      </Card>
    </>
  );
}

/* ---- the two cloud providers ------------------------------------------- */

export function GCPPanel({ form }: { form: FormBinding }) {
  return (
    <Card titleKey="set.gcp">
      <div className="card-pad form-grid">
        <TextField
          form={form}
          control="gcp_project"
          labelKey="set.gcp.project"
          placeholder="my-project"
        />
        <TextField
          form={form}
          control="gcp_region"
          labelKey="set.gcp.region"
          placeholder="us-central1"
        />
        <TextField
          form={form}
          control="gcp_zone"
          labelKey="set.gcp.zone"
          placeholder="us-central1-a"
        />
        <TextField
          form={form}
          control="gcp_key_file"
          labelKey="set.gcp.keyfile"
          placeholder="/var/lib/portage-engine/gcp-key.json"
        />
      </div>
    </Card>
  );
}

export function AWSPanel({ form }: { form: FormBinding }) {
  return (
    <Card titleKey="set.aws">
      <div className="card-pad form-grid">
        <TextField
          form={form}
          control="aws_region"
          labelKey="set.gcp.region"
          placeholder="us-east-1"
        />
        <TextField
          form={form}
          control="aws_zone"
          labelKey="set.gcp.zone"
          placeholder="us-east-1a"
        />
        <TextField form={form} control="aws_access_key" labelKey="set.aws.ak" autoComplete="off" />
        <SecretField form={form} control="aws_secret_key" labelKey="set.aws.sk" />
      </div>
    </Card>
  );
}

/* ---- static builders, mirrors, build config ----------------------------- */

export function BuildersPanel({ form }: { form: FormBinding }) {
  return (
    <Card titleKey="set.sec.builders">
      <div className="card-pad">
        <TextField
          form={form}
          control="remote_builders"
          labelKey="set.builders"
          hintKey="set.builders.hint"
          placeholder="http://builder1:9090,http://builder2:9090"
        />
      </div>
    </Card>
  );
}

export function MirrorsPanel({ form }: { form: FormBinding }) {
  const messages = useMessages();
  return (
    <>
      <Card titleKey="set.sec.mirrors">
        <div className="card-pad">
          <Hint>{messages.t('set.mirrors.hint')}</Hint>
          <div className="form-grid">
            <TextField
              form={form}
              control="gentoo_mirror"
              labelKey="set.mirrors.gentoo"
              hintKey="set.mirrors.gentoo.hint"
              placeholder="http://10.31.0.2/gentoo"
            />
            {/* The two option labels are the sync methods' own names, which is
                what an operator writes into repos.conf and what the server
                stores. The sentence explaining which to pick is the hint below,
                translated in both catalogues — so nothing is lost by not
                dressing an identifier up as prose. */}
            <SelectField
              form={form}
              control="portage_sync_method"
              labelKey="set.mirrors.method"
              hintKey="set.mirrors.method.hint"
              options={[
                { value: 'webrsync', label: 'webrsync' },
                { value: 'rsync', label: 'rsync' },
              ]}
            />
            <TextField
              form={form}
              control="portage_sync_uri"
              labelKey="set.mirrors.sync"
              hintKey="set.mirrors.sync.hint"
              placeholder="rsync://mirror/gentoo-portage"
            />
          </div>
        </div>
      </Card>
      <Card titleKey="set.sec.upload">
        <div className="card-pad">
          <Hint>{messages.t('set.upload.desc')}</Hint>
          <div className="form-grid">
            <TextField
              form={form}
              control="upload_url"
              labelKey="set.upload.url"
              hintKey="set.upload.url.hint"
              placeholder="http://10.31.0.2"
            />
            <TextField
              form={form}
              control="upload_dir"
              labelKey="set.upload.dir"
              hintKey="set.upload.dir.hint"
              placeholder="portage-engine"
            />
            <TextField
              form={form}
              control="upload_user"
              labelKey="set.upload.user"
              autoComplete="off"
            />
            <SecretField
              form={form}
              control="upload_password"
              labelKey="set.upload.pass"
              autoComplete="new-password"
            />
          </div>
        </div>
      </Card>
    </>
  );
}

export function BuildConfPanel({ form }: { form: FormBinding }) {
  const messages = useMessages();
  return (
    <Card titleKey="set.sec.buildconf">
      <div className="card-pad">
        <TextAreaField
          form={form}
          control="make_conf_extra"
          labelKey="set.makeconf"
          hintKey="set.makeconf.hint"
          placeholder={'USE="-doc -test"\nACCEPT_LICENSE="*"\nFEATURES="parallel-fetch"'}
        />
        <TextField
          form={form}
          control="build_features"
          labelKey="set.buildfeatures"
          hintKey="set.buildfeatures.hint"
          placeholder="parallel-fetch"
          spellCheck={false}
        />
        {/* Not a control: the backend is fixed and the server rejects any other
            value, so the card states the mode rather than offering a choice
            that does not exist. The value printed is the one on the wire. */}
        <div className="field">
          <p className="settings-stated-label">{messages.t('set.buildmode')}</p>
          <p className="mono">native-gentoo</p>
          <Hint>{messages.t('set.buildmode.hint')}</Hint>
        </div>
      </div>
    </Card>
  );
}

/* ---- ssh and delivery --------------------------------------------------- */

export function SSHPanel({ form }: { form: FormBinding }) {
  return (
    <Card titleKey="set.sec.ssh">
      <div className="card-pad">
        <div className="form-grid">
          <TextField
            form={form}
            control="ssh_key_path"
            labelKey="set.keypath"
            hintKey="set.keypath.hint"
            placeholder="/var/lib/portage-engine/id_ed25519"
          />
          <TextField
            form={form}
            control="ssh_user"
            labelKey="set.sshuser"
            hintKey="set.sshuser.hint"
            placeholder="root"
          />
          <TextField form={form} control="ssh_known_hosts" labelKey="set.knownhosts" />
        </div>
        <CheckField form={form} control="ssh_insecure" labelKey="set.hostkey" />
      </div>
    </Card>
  );
}

export function NetPanel({ form }: { form: FormBinding }) {
  return (
    <Card titleKey="set.sec.net">
      <div className="card-pad form-grid">
        <TextField
          form={form}
          control="callback"
          labelKey="set.callback"
          hintKey="set.callback.hint"
          placeholder="http://10.0.0.10:8080"
        />
        <TextField
          form={form}
          control="bin_path"
          labelKey="set.binpath"
          hintKey="set.binpath.hint"
          placeholder="bin/portage-builder-linux-amd64"
        />
        <TextField
          form={form}
          control="bin_url"
          labelKey="set.binurl"
          hintKey="set.binurl.hint"
          placeholder="https://example.com/portage-builder-linux-amd64"
        />
        {/* The one field of the forty-five whose label, hint and placeholder
            the catalogue has never carried — the Go page wrote all three into
            the markup with no data-i18n, so they were English on the Chinese
            page as well. Kept at parity and named in the handoffs; the strings
            are gathered in ./text.ts rather than typed in here. */}
        <TextField
          form={form}
          control="bin_sha256"
          label={UNTRANSLATED.binsha}
          hint={UNTRANSLATED.binshaHint}
          maxLength={64}
          placeholder={UNTRANSLATED.binshaPlaceholder}
        />
      </div>
    </Card>
  );
}
