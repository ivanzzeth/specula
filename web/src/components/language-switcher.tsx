import { useTranslation } from 'react-i18next';

import { SUPPORTED_LANGUAGES } from '@/i18n';
import { cn } from '@/lib/utils';

/**
 * LanguageSwitcher — the interface language control in the identity rail.
 *
 * ── WHY A SEGMENTED TOGGLE, NOT A DROPDOWN ───────────────────────────────────
 * Two options. A Select would cost a click to reveal what is already showable,
 * and would put a second dropdown chrome next to the org switcher for something
 * far less consequential. Both languages sit inline, and the active one is
 * AMBER — the same text-colour-as-state language the nav, the tabs and the
 * sortable table headers already speak. No pill, no filled background.
 *
 * Placed after the org switcher and before sign-out: it belongs to "who am I
 * and how am I set up", not to navigation.
 *
 * ── PERSISTENCE ──────────────────────────────────────────────────────────────
 * i18next-browser-languagedetector writes the choice to localStorage
 * (`specula:lang`) via its `caches` option — the click below is the whole
 * mechanism. On the next visit `localStorage` is consulted before `navigator`,
 * so an explicit choice always outranks the browser's preference (see
 * i18n/index.ts `detection.order`). <html lang> follows through the
 * `languageChanged` hook, which is what arms the CJK label treatment in
 * index.css.
 *
 * The label is each language's ENDONYM ("中文", not "Chinese"): someone who
 * needs to switch away from a language they cannot read must still be able to
 * recognise their own.
 */
export function LanguageSwitcher() {
  const { i18n, t } = useTranslation();

  const active = i18n.resolvedLanguage?.toLowerCase().startsWith('zh') ? 'zh-CN' : 'en';

  return (
    <div
      className="flex items-center gap-1"
      role="group"
      aria-label={t('nav.languageTitle')}
      title={t('nav.languageTitle')}
    >
      {SUPPORTED_LANGUAGES.map((lang, i) => {
        const isActive = lang.code === active;
        return (
          <span key={lang.code} className="flex items-center gap-1">
            {i > 0 && <span aria-hidden className="h-2.5 w-px bg-slate-800" />}
            <button
              type="button"
              lang={lang.code}
              aria-current={isActive ? 'true' : undefined}
              // The full name is the accessible name; the rail shows the short
              // label because the rail is 36px and density is the point.
              aria-label={lang.name}
              onClick={() => void i18n.changeLanguage(lang.code)}
              className={cn(
                'label-caps text-micro transition-colors duration-fast',
                'focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring',
                isActive
                  ? 'font-semibold text-brand'
                  : 'text-slate-500 hover:text-slate-200'
              )}
            >
              {lang.label}
            </button>
          </span>
        );
      })}
    </div>
  );
}
