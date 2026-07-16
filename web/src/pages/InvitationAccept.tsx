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
import { useNavigate, useParams } from 'react-router-dom';
import { Check, MailCheck, X } from 'lucide-react';

import { ApiError, acceptInvitation, declineInvitation } from '@/api/client';
import { useAuth } from '@/components/auth';
import { useOrg } from '@/components/org-context';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';

type Outcome = { kind: 'declined' } | { kind: 'error'; message: string };

/** explain turns a server status into something a human can act on. */
function explain(err: unknown): string {
  if (!(err instanceof ApiError)) {
    return 'Could not reach the server. Please retry.';
  }
  switch (err.status) {
    case 403:
      return 'This invitation was sent to a different email address. Sign in as the invited account and open the link again.';
    case 404:
      return 'This invitation link is not valid. Ask whoever invited you to send a new one.';
    case 409:
      return 'This invitation has already been used. If you did not accept it, ask for a new one.';
    case 410:
      return 'This invitation has expired. Ask whoever invited you to send a new one.';
    default:
      return err.detail || err.message;
  }
}

export function InvitationAccept() {
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
      setOutcome({ kind: 'error', message: explain(err) });
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
      setOutcome({ kind: 'error', message: explain(err) });
    }
    setBusy(false);
  }

  if (outcome?.kind === 'declined') {
    return (
      <Shell title="Invitation declined">
        <p className="text-data text-slate-400">
          You declined this invitation. No membership was created.
        </p>
        <Button className="mt-5" variant="ghost" onClick={() => navigate('/', { replace: true })}>
          Back
        </Button>
      </Shell>
    );
  }

  return (
    <Shell title="You have been invited">
      <p className="text-data text-slate-400">
        Accepting adds this account to the organisation. You will be able to switch to it from
        the organisation switcher.
      </p>

      <dl className="mt-5 space-y-1 border-t border-slate-800 pt-4 text-left">
        <div className="flex items-baseline justify-between gap-4">
          <dt className="text-micro uppercase tracking-wider text-slate-500">Accepting as</dt>
          <dd className="text-data text-slate-100">{user?.email ?? '—'}</dd>
        </div>
      </dl>
      <p className="mt-2 text-left text-micro text-slate-500">
        An invitation is addressed to one address. If this is not the invited one, sign in as
        that account first.
      </p>

      {outcome?.kind === 'error' && (
        <p role="alert" className="mt-4 text-left text-data text-health-blocked">
          {outcome.message}
        </p>
      )}

      <div className="mt-6 flex items-center justify-end gap-2">
        <Button variant="ghost" onClick={decline} disabled={busy}>
          <X aria-hidden className="size-3.5" />
          Decline
        </Button>
        <Button onClick={accept} disabled={busy}>
          <Check aria-hidden className="size-3.5" />
          {busy ? 'Working…' : 'Accept invitation'}
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
