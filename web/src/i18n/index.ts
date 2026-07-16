import i18n from 'i18next';
import { initReactI18next } from 'react-i18next';
import LanguageDetector from 'i18next-browser-languagedetector';

import enCommon from './locales/en/common.json';
import enRegistry from './locales/en/registry.json';
import enCache from './locales/en/cache.json';
import enOps from './locales/en/ops.json';
import enTenancy from './locales/en/tenancy.json';

import zhCommon from './locales/zh-CN/common.json';
import zhRegistry from './locales/zh-CN/registry.json';
import zhCache from './locales/zh-CN/cache.json';
import zhOps from './locales/zh-CN/ops.json';
import zhTenancy from './locales/zh-CN/tenancy.json';

/**
 * Locale files are split along the SAME zone boundaries as App.tsx's ownership
 * map (registry / cache / ops / tenancy), so the people editing a zone's pages
 * edit that zone's copy and nothing else. The merge is shallow and safe because
 * each file owns disjoint top-level keys; `common` is the only shared file.
 */
const en = { ...enCommon, ...enRegistry, ...enCache, ...enOps, ...enTenancy };
const zhCN = { ...zhCommon, ...zhRegistry, ...zhCache, ...zhOps, ...zhTenancy };

/**
 * i18n — English + 简体中文.
 *
 * ── SCOPE (be honest about the boundary) ─────────────────────────────────────
 * This layer translates the CLIENT. The API returns `{"error": "..."}` in
 * English and the UI surfaces those strings verbatim; there is no backend i18n
 * and this file does not pretend otherwise. `src/i18n/server-errors.ts` maps a
 * small, explicit allow-list of common API errors, and anything unmapped is
 * shown as the server sent it. See that file's header for the full rationale.
 *
 * ── TYPOGRAPHY IS PART OF THIS ───────────────────────────────────────────────
 * Two things about the "industrial instrument panel" direction (REGISTRY-DESIGN
 * §5.0) break under Chinese, and are handled deliberately, not papered over:
 *
 *   1. IBM Plex Mono has no CJK glyphs. `scripts/subset-cjk.py` self-hosts a
 *      subset Noto Sans SC so CJK renders in a face WE chose rather than
 *      whatever the OS picks. See index.css `@font-face` + that script's header.
 *
 *   2. `uppercase tracking-wider` is an English-only hierarchy device —
 *      text-transform is a no-op on han glyphs and letter-spacing breaks their
 *      em grid. See index.css `.label-caps`, which is language-aware.
 *
 * ── KEY CONVENTIONS ──────────────────────────────────────────────────────────
 *   · one `translation` namespace, nested by page/zone (nav.*, repos.*, …)
 *   · `common.*` for strings shared across pages (Cancel, Delete, Loading…)
 *   · Terms developers actually say in English in a Chinese context — digest,
 *     manifest, tag, registry, push, pull, token, blob — STAY English in the
 *     zh-CN copy. Translating them would be less clear, not more.
 *   · Chinese copy is terse to match the English. This is an operator tool, not
 *     marketing: no 您好, no 请稍候片刻, no politeness padding.
 */

export const SUPPORTED_LANGUAGES = [
  { code: 'en', label: 'EN', name: 'English' },
  { code: 'zh-CN', label: '中文', name: '简体中文' },
] as const;

export type LanguageCode = (typeof SUPPORTED_LANGUAGES)[number]['code'];

/** localStorage key holding the explicit user choice. */
export const LANGUAGE_STORAGE_KEY = 'specula:lang';

void i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources: {
      en: { translation: en },
      'zh-CN': { translation: zhCN },
    },
    /**
     * Any zh variant (zh, zh-TW, zh-HK, zh-Hans, zh-Hans-CN…) resolves to zh-CN
     * rather than silently falling back to English. We only ship Simplified;
     * serving it to a zh-TW reader is imperfect but far better than an English
     * wall. Everything else lands on English.
     *
     * This MUST be a function, not `nonExplicitSupportedLngs: true`. That option
     * does the opposite of what its name suggests here: it accepts VARIANTS when
     * the BASE is listed (list `zh` → `zh-CN` is accepted). Our list contains
     * `zh-CN`, so it checked `zh-CN` against the stripped `zh`, found nothing,
     * and rejected the language — changeLanguage('zh-CN') silently resolved back
     * to `en` while `getResource` still returned Chinese, so the bundle looked
     * fine and only the UI stayed English. Verified against every zh-* tag above.
     */
    fallbackLng: (code?: string) =>
      code && code.toLowerCase().startsWith('zh') ? ['zh-CN', 'en'] : ['en'],
    supportedLngs: ['en', 'zh-CN'],
    load: 'currentOnly',
    detection: {
      // Explicit choice wins; browser preference only decides the first visit.
      order: ['localStorage', 'navigator'],
      lookupLocalStorage: LANGUAGE_STORAGE_KEY,
      caches: ['localStorage'],
    },
    interpolation: {
      // React already escapes.
      escapeValue: false,
    },
    react: {
      useSuspense: false,
    },
  });

/**
 * Keep <html lang> in step with the active language.
 *
 * This is not bookkeeping — it is load-bearing:
 *   · index.css keys the language-aware label treatment off `html[lang^='zh']`
 *   · it selects the right CJK font stack
 *   · screen readers switch voice on it
 */
function syncHtmlLang(lng: string): void {
  document.documentElement.lang = lng.toLowerCase().startsWith('zh') ? 'zh-CN' : 'en';
}

syncHtmlLang(i18n.resolvedLanguage ?? 'en');
i18n.on('languageChanged', syncHtmlLang);

export default i18n;
