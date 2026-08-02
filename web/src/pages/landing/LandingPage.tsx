/**
 * The landing page.
 *
 * Two things changed in the port, and both are structural.
 *
 * It no longer carries a navigation bar of its own. The console this replaces
 * had two public navs — one hardcoded into the landing template and one built
 * by `publicPage` for /packages, /docs and /status — with the same three links
 * spelled twice, no `aria-current` on the landing copy, and nothing keeping
 * them in step. This page now renders inside the one `PublicChrome`, so the
 * bar, the language control, the sign-in button and the footer are the same
 * nodes an anonymous reader met on the previous page.
 *
 * And the workflow steps are grid items that are allowed to be narrower than
 * the command line inside them. A grid item's automatic minimum size is its
 * min-content width, and a nowrap command is a single unbreakable run: the
 * track could not shrink below it, so the whole document scrolled sideways at
 * every width in [320, 353] and again in [801, 929] where the three-column
 * layout is still on. `min-width: 0` on the step plus a wrapping command is the
 * pair that fixes it, and neither half works alone.
 */

import { Link } from 'react-router';

import { useMessages } from '../../i18n/context';
import type { MessageKey } from '../../i18n/messages';
import { useDocumentTitle } from '../../app/useDocumentTitle';
import '../../styles/pages/landing.css';

/** The three feature cards, in the order the page reads them. */
const FEATURES: readonly { eyebrow: MessageKey; title: MessageKey; text: MessageKey }[] = [
  { eyebrow: 'landing.f1.eyebrow', title: 'landing.f1.title', text: 'landing.f1.text' },
  { eyebrow: 'landing.f2.eyebrow', title: 'landing.f2.title', text: 'landing.f2.text' },
  { eyebrow: 'landing.f3.eyebrow', title: 'landing.f3.title', text: 'landing.f3.text' },
];

/**
 * The three workflow steps.
 *
 * The command lines are literal on purpose: they are the shape of an
 * invocation, not a command to copy against this deployment. The commands that
 * are meant to be copied — the ones carrying this server's host, profile id and
 * binhost path — live on /docs, where they are templated from the binhost
 * inventory rather than written out.
 */
const STEPS: readonly { title: MessageKey; text: MessageKey; command: string }[] = [
  {
    title: 'landing.s1.t',
    text: 'landing.s1.d',
    command: 'portage-client build -package app-misc/jq',
  },
  {
    title: 'landing.s2.t',
    text: 'landing.s2.d',
    command: 'provision → emerge → collect → index',
  },
  {
    title: 'landing.s3.t',
    text: 'landing.s3.d',
    command: 'emerge --getbinpkg app-misc/jq',
  },
];

export function LandingPage() {
  const messages = useMessages();
  useDocumentTitle('title.landing');

  return (
    <div className="landing-page">
      <header className="landing-hero">
        <p className="eyebrow">{messages.t('landing.eyebrow')}</p>
        <h1>{messages.t('landing.h1')}</h1>
        <p className="sub">{messages.t('landing.sub')}</p>
        <div className="cta">
          <Link className="btn blue" to="/overview">
            {messages.t('landing.cta')}
          </Link>
          <Link className="btn" to="/docs">
            {messages.t('landing.docs')}
          </Link>
        </div>
      </header>

      {/* No aria-label on this one: the label the old template carried was the
          English literal "Features", which is the copy-bearing attribute nobody
          sees on screen and therefore the one that shipped untranslated. A
          section with no accessible name is not a landmark, which is the honest
          answer for three cards that are already read in order. */}
      <section className="landing-grid">
        {FEATURES.map((feature) => (
          <article className="landing-card" key={feature.title}>
            <h4>{messages.t(feature.eyebrow)}</h4>
            <h3>{messages.t(feature.title)}</h3>
            <p>{messages.t(feature.text)}</p>
          </article>
        ))}
      </section>

      <section className="landing-flow">
        <h2>{messages.t('landing.flow')}</h2>
        <div className="steps">
          {STEPS.map((step) => (
            <div className="step" key={step.title}>
              <h4>{messages.t(step.title)}</h4>
              <p>{messages.t(step.text)}</p>
              <span className="mono">{step.command}</span>
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}
