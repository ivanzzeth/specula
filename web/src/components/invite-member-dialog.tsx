/**
 * InviteMemberDialog — create an invitation and hand back its link.
 *
 * The distinction from "Add member" is consent, and it is the whole point:
 * adding writes a membership immediately, while inviting writes nothing until
 * the invitee themselves accepts. Membership is what grants push access, so the
 * invitation path is the one that lets the other party agree first.
 *
 * Specula does not send email, so the link IS the delivery mechanism — the
 * server returns the token exactly once, on creation, and this dialog is the
 * only place it is ever shown. If the admin closes this without copying it, the
 * invitation is unreachable and must be recreated. The UI says so plainly
 * rather than letting them discover it later.
 *
 * Design: instrument-panel — the token link is a mono readout in a hairline
 * well, amber reserved for the copy affordance that matters.
 */
import { type FormEvent, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Check, Copy, Send } from 'lucide-react';

import { ApiError, createInvitation } from '@/api/client';
import { translateServerError } from '@/i18n/server-errors';
import { Button } from '@/components/ui/button';
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';

/** owner is intentionally absent: ownership is not grantable by invitation. */
const INVITE_ROLES = ['viewer', 'editor', 'admin'] as const;

interface InviteMemberDialogProps {
  orgId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function InviteMemberDialog({ orgId, open, onOpenChange }: InviteMemberDialogProps) {
  const { t } = useTranslation();
  const [email, setEmail] = useState('');
  const [role, setRole] = useState<string>('viewer');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [link, setLink] = useState('');
  const [copied, setCopied] = useState(false);

  function reset() {
    setEmail('');
    setRole('viewer');
    setBusy(false);
    setError('');
    setLink('');
    setCopied(false);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!email.trim() || busy) return;
    setBusy(true);
    setError('');
    try {
      const inv = await createInvitation(orgId, { email: email.trim(), role });
      if (!inv.token) {
        setError(t('invitations.dialog.noToken'));
        setBusy(false);
        return;
      }
      setLink(`${window.location.origin}/invitations/${encodeURIComponent(inv.token)}`);
    } catch (err) {
      setError(
        err instanceof ApiError
          ? translateServerError(err.detail) || err.message
          : t('invitations.dialog.failed')
      );
    }
    setBusy(false);
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(link);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      setCopied(false);
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) reset();
        onOpenChange(next);
      }}
    >
      <DialogContent className="sm:max-w-lg">
        {link ? (
          <>
            <DialogHeader>
              <DialogTitle>{t('invitations.dialog.createdTitle')}</DialogTitle>
              <DialogDescription>
                {t('invitations.dialog.createdDescription', { email })}
              </DialogDescription>
            </DialogHeader>

            <div className="my-4 rounded border border-slate-800 bg-slate-950 p-3">
              <code className="block break-all text-data text-slate-300">{link}</code>
            </div>

            <DialogFooter>
              <Button variant="ghost" onClick={() => onOpenChange(false)}>
                {t('invitations.dialog.done')}
              </Button>
              <Button onClick={copy}>
                {copied ? <Check aria-hidden className="size-3.5" /> : <Copy aria-hidden className="size-3.5" />}
                {copied ? t('common.copied') : t('invitations.dialog.copyLink')}
              </Button>
            </DialogFooter>
          </>
        ) : (
          <form onSubmit={submit}>
            <DialogHeader>
              <DialogTitle>{t('invitations.dialog.title')}</DialogTitle>
              <DialogDescription>{t('invitations.dialog.description')}</DialogDescription>
            </DialogHeader>

            <div className="space-y-4 py-4">
              <div className="space-y-2">
                <Label htmlFor="invite-email">{t('invitations.dialog.email')}</Label>
                <Input
                  id="invite-email"
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  placeholder={t('invitations.dialog.emailPlaceholder')}
                  autoFocus
                  required
                />
              </div>
              <div className="space-y-2">
                <Label htmlFor="invite-role">{t('invitations.dialog.role')}</Label>
                <Select value={role} onValueChange={setRole}>
                  <SelectTrigger id="invite-role">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {INVITE_ROLES.map((r) => (
                      <SelectItem key={r} value={r}>
                        {r}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                <p className="text-micro text-slate-500">{t('invitations.dialog.roleHint')}</p>
              </div>
            </div>

            {error && (
              <p role="alert" className="pb-2 text-data text-health-blocked">
                {error}
              </p>
            )}

            <DialogFooter>
              <Button type="button" variant="ghost" onClick={() => onOpenChange(false)} disabled={busy}>
                {t('common.cancel')}
              </Button>
              <Button type="submit" disabled={busy || !email.trim()}>
                <Send aria-hidden className="size-3.5" />
                {busy ? t('invitations.dialog.busy') : t('invitations.dialog.submit')}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  );
}
