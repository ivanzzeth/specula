import i18n from './index';

/**
 * Client-side translation for a SMALL, EXPLICIT allow-list of API errors.
 *
 * ── THE HONEST BOUNDARY ──────────────────────────────────────────────────────
 * The Go control plane has no i18n layer and returns `{"error": "..."}` in
 * English. Building a backend i18n layer is out of scope for this change, and
 * faking one here would be worse than useless — `internal/admin` alone emits
 * ~105 distinct error strings, most of them 500-level internals ("failed to
 * list keys"), protocol codes (`BLOB_UPLOAD_INVALID`), or %w-wrapped chains
 * carrying dynamic values. Mapping all of them client-side would be a
 * permanently-drifting lie: the moment the server reworded one, the UI would
 * silently fall back to English with no test to catch it.
 *
 * So we map ONLY errors that satisfy all three conditions:
 *
 *   1. a user can actually CAUSE it (validation / permission / conflict —
 *      not "the database is down")
 *   2. a user can actually ACT on it (retype the password, ask for a role)
 *   3. the string is a STABLE constant with no interpolated values
 *
 * That is the ~24 entries below — overwhelmingly the login/register path, which
 * is where a user who needs Chinese is most likely to be stuck and least able
 * to recover. Everything else passes through VERBATIM in English, on purpose.
 * A precise English error beats a vague translated one.
 *
 * DELIBERATELY NOT MAPPED:
 *   · `failed to *` (≈45 strings) — internal faults; the English is a support
 *     keyword, and translating it would make bug reports harder to triage.
 *   · the org-quota error — it interpolates counts ("you have already created
 *     %d of a maximum %d organisation(s)"), so it is not a stable constant.
 *   · OCI/npm/pypi protocol errors — those are consumed by docker/npm/pip,
 *     never rendered by this UI.
 *
 * Matching is exact on the lower-cased, trimmed string. No fuzzy or prefix
 * matching: a near-miss that silently mistranslates an error is the one failure
 * mode worse than showing English.
 */

/** Locale keys live under `errors.server.*` in each locale's `common.json`. */
const MAPPED: Record<string, string> = {
  // ── auth: the highest-traffic, highest-stakes path ─────────────────────────
  'invalid email or password': 'invalidCredentials',
  'email already registered': 'emailTaken',
  'email already taken': 'emailTaken',
  'email is required': 'emailRequired',
  'login failed': 'loginFailed',
  'registration failed': 'registrationFailed',
  unauthorized: 'unauthorized',
  forbidden: 'forbidden',

  // ── generic resource states ────────────────────────────────────────────────
  'not found': 'notFound',
  'user not found': 'userNotFound',
  'org not found': 'orgNotFound',
  'repo not found': 'repoNotFound',
  'member not found': 'memberNotFound',
  'key not found': 'keyNotFound',
  'name is required': 'nameRequired',

  // ── membership / role guards ───────────────────────────────────────────────
  'not a member of this org': 'notAMember',
  'not a member of this organization': 'notAMember',
  'org admin role required': 'orgAdminRequired',
  'org owner role required for ownership changes': 'orgOwnerRequired',
  'cannot delete the last admin': 'lastAdmin',
  'cannot demote the last admin': 'lastAdminDemote',
  'cannot delete your own account': 'selfDelete',

  // ── invitations ────────────────────────────────────────────────────────────
  'invitation not found': 'inviteNotFound',
  'invitation has expired': 'inviteExpired',
  'invitation is no longer pending': 'inviteNotPending',
  'invitation is addressed to a different email': 'inviteWrongEmail',
};

/**
 * translateServerError maps a server `error` string to localised copy when it
 * is on the allow-list, and otherwise returns it unchanged (English).
 *
 * Always safe to call: in English it is effectively a pass-through, because
 * every mapped key's `en` value is the server's own wording.
 */
export function translateServerError(detail: string | undefined | null): string {
  if (!detail) return '';
  const key = MAPPED[detail.trim().toLowerCase()];
  if (!key) return detail;
  return i18n.t(`errors.server.${key}`, { defaultValue: detail });
}
