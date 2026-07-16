/**
 * InvitationAccept — /invitations/:token
 *
 * Where an invitation link lands. Mounted under RequireAuth but deliberately
 * NOT under RequireOrg: the invitee has no org yet — that is the entire point —
 * so an org guard here would bounce them off the page that fixes it.
 *
 * There is no preview endpoint (an unauthenticated read of an invitation would
 * leak org membership to anyone holding a guessed token), so the page cannot
 * name the org before acceptance. It states what it knows — an invitation, and
 * which account is about to accept it — and lets the server adjudicate.
 *
 * The account shown is load-bearing: an invitation is addressed to one email,
 * and the most common failure is being logged in as somebody else. Showing the
 * signed-in address up front turns a 403 into something the user can predict
 * and fix before clicking.
 *
 * Server responses this page distinguishes:
 *   200 — joined; switch into the org and go
 *   403 — addressed to a different email
 *   404 — no such invitation
 *   409 — already accepted or declined
 *   410 — expired
 */
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate, useParams } from 'react-router-dom';
import { Check, MailCheck, X } from 'lucide-react';

import { ApiError, acceptInvitation, declineInvitation } from '@/api/client';
import { translateServerError } from '@/i18n/server-errors';
import { useAuth } from '@/components/auth';
import { useOrg } from '@/components/org-context';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';

type Outcome = { kind: 'declined' } | { kind: 'error'; message: string };

type TFn = ReturnType<typeof useTranslation>['t'];

/**
 * explain turns a server status into something a human can act on.
 *
 * The four states below are decided from the HTTP status, not the body, so they
 * are fully translatable here; anything else falls back to the server's own
 * `error` string via the shared allow-list.
 */
function explain(err: unknown, t: TFn): string {
  if (!(err instanceof ApiError)) {
    return t('invitations.error.network');
  }
  switch (err.status) {
    case 403:
      return t('invitations.error.wrongEmail');
    case 404:
      return t('invitations.error.notFound');
    case 409:
      return t('invitations.error.used');
    case 410:
      return t('invitations.error.expired');
    default:
      return translateServerError(err.detail) || err.message;
  }
}

export function InvitationAccept() {
  const { t } = useTranslation();
  const { token = '' } = useParams();
  const { user } = useAuth();
  const { refresh, switchOrg } = useOrg();
  const navigate = useNavigate();

  const [busy, setBusy] = useState(false);
  const [outcome, setOutcome] = useState<Outcome | null>(null);

  async function accept() {
    setBusy(true);
    setOutcome(null);
    try {
      const member = await acceptInvitation(token);
      // Enter the org that was just joined: the membership list the switcher
      // renders from is stale until refreshed.
      refresh();
      switchOrg(member.org_id);
      navigate('/', { replace: true });
    } catch (err) {
      setOutcome({ kind: 'error', message: explain(err, t) });
      setBusy(false);
    }
  }

  async function decline() {
    setBusy(true);
    setOutcome(null);
    try {
      await declineInvitation(token);
      setOutcome({ kind: 'declined' });
    } catch (err) {
      setOutcome({ kind: 'error', message: explain(err, t) });
    }
    setBusy(false);
  }

  if (outcome?.kind === 'declined') {
    return (
      <Shell title={t('invitations.declinedTitle')}>
        <p className="text-data text-slate-400">{t('invitations.declinedBody')}</p>
        <Button className="mt-5" variant="ghost" onClick={() => navigate('/', { replace: true })}>
          {t('common.back')}
        </Button>
      </Shell>
    );
  }

  return (
    <Shell title={t('invitations.invitedTitle')}>
      <p className="text-data text-slate-400">{t('invitations.invitedBody')}</p>

      <dl className="mt-5 space-y-1 border-t border-slate-800 pt-4 text-left">
        <div className="flex items-baseline justify-between gap-4">
          <dt className="label-caps text-micro text-slate-500">{t('invitations.acceptingAs')}</dt>
          <dd className="text-data text-slate-100">{user?.email ?? '—'}</dd>
        </div>
      </dl>
      <p className="mt-2 text-left text-micro text-slate-500">
        {t('invitations.acceptingAsHint')}
      </p>

      {outcome?.kind === 'error' && (
        <p role="alert" className="mt-4 text-left text-data text-health-blocked">
          {outcome.message}
        </p>
      )}

      <div className="mt-6 flex items-center justify-end gap-2">
        <Button variant="ghost" onClick={decline} disabled={busy}>
          <X aria-hidden className="size-3.5" />
          {t('invitations.decline')}
        </Button>
        <Button onClick={accept} disabled={busy}>
          <Check aria-hidden className="size-3.5" />
          {busy ? t('invitations.busy') : t('invitations.accept')}
        </Button>
      </div>
    </Shell>
  );
}

function Shell({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="mx-auto max-w-md py-16">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <MailCheck aria-hidden className="size-4 text-brand" />
            {title}
          </CardTitle>
        </CardHeader>
        <CardContent>{children}</CardContent>
      </Card>
    </div>
  );
}
