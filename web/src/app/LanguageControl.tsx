import { useMessages } from '../i18n/context';
import type { Language } from '../i18n/messages';

/**
 * The language control, in every chrome including the public one.
 *
 * It goes missing from public layouts even in projects that have one shell, and
 * the cost lands on exactly the reader who cannot fix it themselves: someone
 * resolved into a language they do not read by `Accept-Language`, on a page with
 * no session and no way out of it.
 *
 * The label is the language it switches TO, always written in that language.
 * Writing it in the current language ("Chinese") asks a reader who cannot read
 * the current language to read the current language.
 */

const OTHER: Record<Language, { lang: Language; label: string }> = {
  en: { lang: 'zh', label: '中文' },
  zh: { lang: 'en', label: 'English' },
};

export function LanguageControl({ onChange }: { onChange: (lang: Language) => void }) {
  const messages = useMessages();
  const other = OTHER[messages.lang];
  return (
    <button
      className="lang-btn"
      type="button"
      lang={other.lang === 'zh' ? 'zh-CN' : 'en'}
      onClick={() => {
        onChange(other.lang);
      }}
    >
      {other.label}
    </button>
  );
}
