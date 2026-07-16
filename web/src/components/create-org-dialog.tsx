/**
 * CreateOrgDialog — self-service organization creation.
 *
 * Organizations are invitation-only, which leaves a user who has not been
 * invited with no way into the product at all. This is that way in: they create
 * their own org and own it. It is reachable from the org switcher and from
 * RequireOrg's empty state — the two places a user actually notices they have
 * no org.
 *
 * Only a name is asked for. The server derives the slug (the namespace that
 * appears in a pull reference), so the form does not make a user invent a
 * URL-safe identifier before they have done anything.
 *
 * Design: instrument-panel — hairline panel, near-square, amber only on the
 * confirming action, the derived slug previewed in mono as the registry path
 * fragment it will become.
 */
import { type FormEvent, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Building2 } from 'lucide-react';

import { createOrg } from '@/api/client';
import { ApiError } from '@/api/client';
import { translateServerError } from '@/i18n/server-errors';
import type { OrgDTO } from '@/api/types';
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

/**
 * slugPreview mirrors the server's slugify so the user sees the namespace they
 * are about to get. It is a preview only — the server remains authoritative.
 */
export function slugPreview(name: string): string {
  return name
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
}

interface CreateOrgDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Called with the created org once it exists and the caller owns it. */
  onCreated: (org: OrgDTO) => void;
}

export function CreateOrgDialog({ open, onOpenChange, onCreated }: CreateOrgDialogProps) {
  const { t } = useTranslation();
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  const slug = slugPreview(name);

  function reset() {
    setName('');
    setError('');
    setBusy(false);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!name.trim() || busy) return;
    setBusy(true);
    setError('');
    try {
      // Slug omitted deliberately: the server derives it from the name.
      const org = await createOrg({ name: name.trim() });
      reset();
      onCreated(org);
      onOpenChange(false);
    } catch (err) {
      setError(
        err instanceof ApiError
          ? translateServerError(err.detail) || err.message
          : t('orgDialog.failed')
      );
      setBusy(false);
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
      <DialogContent className="sm:max-w-md">
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <Building2 aria-hidden className="size-4 text-brand" />
              {t('orgDialog.title')}
            </DialogTitle>
            <DialogDescription>{t('orgDialog.description')}</DialogDescription>
          </DialogHeader>

          <div className="space-y-2 py-4">
            <Label htmlFor="new-org-name">{t('orgDialog.name')}</Label>
            <Input
              id="new-org-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t('orgDialog.namePlaceholder')}
              autoFocus
              autoComplete="off"
              required
              aria-describedby="new-org-slug-hint"
            />
            <p id="new-org-slug-hint" className="text-micro text-slate-500">
              {slug ? (
                <>
                  {t('orgDialog.namespace')}{' '}
                  <span className="text-slate-300">
                    {slug}/<span className="text-slate-600">&lt;repo&gt;</span>
                  </span>
                </>
              ) : (
                t('orgDialog.namespaceHint')
              )}
            </p>
          </div>

          {error && (
            <p role="alert" className="pb-2 text-data text-health-blocked">
              {error}
            </p>
          )}

          <DialogFooter>
            <Button
              type="button"
              variant="ghost"
              onClick={() => onOpenChange(false)}
              disabled={busy}
            >
              {t('common.cancel')}
            </Button>
            <Button type="submit" disabled={busy || !slug}>
              {busy ? t('orgDialog.busy') : t('orgDialog.submit')}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
