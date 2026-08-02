/**
 * Which federated destinations the sign-in card offers, and what each is called.
 *
 * The name is the whole reason this is a function. In the console this replaces
 * the provider button was `'Sign in with ' + provider.DisplayName` assembled in
 * page script against a catalogue entry that carried no slot for the name, so on
 * a deployment with SSO as the only sign-in method and Chinese as the served
 * language, the one operable control on the page was in English. Here the label
 * is a key plus its values and the message layer does the interpolation, which
 * is the only arrangement the placeholder-parity check in i18n.test.ts can see.
 *
 * The unnamed branch is not a fallback for a missing translation — it is the
 * correct sentence for a deployment the console has not been told the provider
 * names of, and it is written in both languages.
 */

import type { BootIdentityProvider } from '../../boot/payload';
import type { MessageKey, MessageValues } from '../../i18n/messages';
import { LEGACY_PROVIDER_START, loginURL, providerStartURL } from './urls';

export interface ProviderChoice {
  /** React key; also the provider id the start route is built from. */
  key: string;
  href: string;
  labelKey: MessageKey;
  /** Present only when the label has a slot to fill. */
  labelValues?: MessageValues;
}

export function providerChoices(
  oidcEnabled: boolean,
  named: readonly BootIdentityProvider[],
  context: { stepUp: boolean; returnTo: string },
): ProviderChoice[] {
  if (!oidcEnabled) {
    return [];
  }
  // A step-up asks the provider to prove the reader again, so one that cannot be
  // asked is dropped from the card rather than offered and answered with the
  // session already in hand — the rule pageData applies in
  // internal/dashboard/dashboard.go, applied here because the console asks for a
  // step-up from the shell and the device page as well as from this card.
  const offered = context.stepUp ? named.filter((provider) => provider.supports_step_up) : named;
  if (offered.length === 0) {
    // One destination, unnamed, because that is exactly what the console knows:
    // the boot payload says a federated sign-in is available and names nobody
    // it can build a start route for. The sentence says "an identity provider"
    // in both languages rather than inventing a proper noun for something the
    // reader is about to be sent to.
    //
    // Reached two ways, as in the console this replaces: a deployment configured
    // the pre-multi-provider way sends no names at all, and a deployment whose
    // only provider cannot step up has its list emptied by the filter above. The
    // server resolves this route against its first configured provider, so it is
    // the one federated destination that exists in both cases.
    return [
      {
        key: 'oidc',
        href: loginURL({ loginPath: LEGACY_PROVIDER_START, ...context }),
        labelKey: 'device.federated.signin',
      },
    ];
  }
  return offered.map((provider) => ({
    key: provider.id,
    href: loginURL({ loginPath: providerStartURL(provider.id), ...context }),
    labelKey: 'login.oidc',
    labelValues: { provider: provider.display_name },
  }));
}
