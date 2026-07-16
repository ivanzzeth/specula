import { useCallback, useEffect, useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { Link, useParams } from 'react-router-dom';

import { ApiError, deleteRepoTag, getRepo, listRepoTags, patchRepo } from '@/api/client';
import type { RepoDTO, TagDTO } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { VisibilityBadge } from '@/components/ui/badge';
import { SkeletonRows } from '@/components/ui/skeleton';
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  EmptyRow,
} from '@/components/ui/table';
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog';
import { useOrg } from '@/components/org-context';
import { useToast } from '@/hooks/use-toast';
import { translateServerError } from '@/i18n/server-errors';
import { formatBytes, formatRelative, formatUnix } from '@/lib/utils';
import { useRegistryHost } from '../hooks/useRegistryHost';

/**
 * errText — the message to show for a failed request.
 *
 * ApiError carries the server's raw English `detail`; translateServerError
 * localises the small allow-list of user-actionable errors and passes anything
 * else through verbatim (see src/i18n/server-errors.ts for why).
 */
function errText(e: unknown): string {
  if (e instanceof ApiError) return translateServerError(e.detail) || e.message;
  return e instanceof Error ? e.message : String(e);
}

/** Convert an RFC3339 date string to Unix seconds. Returns 0 for missing/invalid. */
function toUnix(s: string | undefined): number {
  return s ? Math.floor(new Date(s).getTime() / 1000) : 0;
}

/**
 * shortDigest trims a "sha256:abcdef…" digest for display.
 * Shows "sha256:abcdef012345…" — enough to identify, short enough to scan.
 */
function shortDigest(digest: string): string {
  if (!digest) return '—';
  const colon = digest.indexOf(':');
  if (colon < 0) return `${digest.slice(0, 14)}…`;
  const algo = digest.slice(0, colon + 1);
  const hash = digest.slice(colon + 1);
  return `${algo}${hash.slice(0, 12)}…`;
}

/**
 * CopyButton — clipboard copy with a transient "✓" confirmation.
 * Silently degrades in insecure contexts where the clipboard API is unavailable.
 */
function CopyButton({ text }: { text: string }) {
  const [done, setDone] = useState(false);
  const { t } = useTranslation();
  return (
    <Button
      size="sm"
      variant="outline"
      // Keep an accessible name while the label is the transient "✓".
      aria-label={done ? t('common.copied') : t('common.copy')}
      onClick={() => {
        void navigator.clipboard
          .writeText(text)
          .then(() => {
            setDone(true);
            setTimeout(() => setDone(false), 1800);
          })
          .catch(() => {
            // clipboard API unavailable — degrade silently
          });
      }}
    >
      {done ? '✓' : t('common.copy')}
    </Button>
  );
}

/**
 * Repository detail + tags — REGISTRY-DESIGN §5.1.
 *
 * - Shows repo name, visibility badge, created time, stats.
 * - A prominent copy-able `docker pull` command prefilled with the latest tag.
 * - Dense tags table: each row has its own per-tag pull command copy button.
 * - Admin/owner: visibility toggle (PATCH) and per-tag delete (with confirmation).
 *
 * Honesty:
 *  - arch is ALWAYS rendered "—" (nothing parses the image config blob).
 *  - size is manifest size, not image pull size — labelled accordingly.
 *
 * Owned by: Agent 1 · Registry.
 */
export function RepoDetail() {
  const { repo: repoParam } = useParams<{ repo: string }>();
  const { activeOrg, canAdminOrg } = useOrg();
  const { toast } = useToast();
  const { t } = useTranslation();

  const [repo, setRepo] = useState<RepoDTO | null>(null);
  const [tags, setTags] = useState<TagDTO[]>([]);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(true);
  const [busyTag, setBusyTag] = useState<string | null>(null);
  const [busyVis, setBusyVis] = useState(false);

  const load = useCallback(() => {
    const org = activeOrg;
    const repoName = repoParam;
    if (!org || !repoName) return;
    setLoading(true);
    setErr('');
    Promise.all([getRepo(org.slug, repoName), listRepoTags(org.slug, repoName)])
      .then(([repoData, tagsData]) => {
        setRepo(repoData);
        setTags(tagsData.tags ?? []);
      })
      .catch((e: unknown) => setErr(errText(e)))
      .finally(() => setLoading(false));
  }, [activeOrg, repoParam]);

  useEffect(load, [load]);

  const onDeleteTag = async (tag: string) => {
    const org = activeOrg;
    const repoName = repoParam;
    const current = repo;
    if (!org || !repoName || !current) return;
    setBusyTag(tag);
    try {
      await deleteRepoTag(org.slug, repoName, tag);
      setTags((prev) => prev.filter((t) => t.tag !== tag));
      // Keep the tag_count in sync so the header stat stays accurate
      setRepo({ ...current, tag_count: Math.max(0, current.tag_count - 1) });
      toast({ variant: 'success', title: t('repoDetail.toast.tagDeleted'), description: tag });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: t('repoDetail.toast.tagDeleteFailed'),
        description: errText(e),
        duration: Infinity,
      });
    } finally {
      setBusyTag(null);
    }
  };

  const onToggleVisibility = async () => {
    const org = activeOrg;
    const repoName = repoParam;
    const current = repo;
    if (!org || !repoName || !current) return;
    setBusyVis(true);
    const next = current.visibility === 'public' ? 'private' : 'public';
    try {
      const updated = await patchRepo(org.slug, repoName, { visibility: next });
      setRepo(updated);
      toast({
        variant: 'success',
        title:
          next === 'public'
            ? t('repoDetail.toast.nowPublic')
            : t('repoDetail.toast.nowPrivate'),
        description:
          next === 'public'
            ? t('repoDetail.toast.nowPublicDesc')
            : t('repoDetail.toast.nowPrivateDesc'),
      });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: t('repoDetail.toast.visibilityFailed'),
        description: errText(e),
        duration: Infinity,
      });
    } finally {
      setBusyVis(false);
    }
  };

  // Hooks must run on every render path: this sits above the early returns
  // below, not beside its only use further down. Called conditionally, React's
  // hook order diverges between the loading/error/no-org branches and the happy
  // path — a rules-of-hooks violation that breaks state, not merely a lint nit.
  const host = useRegistryHost();

  if (!activeOrg) {
    return (
      <Card>
        <CardContent className="text-data text-slate-400">
          {t('repos.noActiveOrg')}
        </CardContent>
      </Card>
    );
  }

  if (loading) {
    return (
      <div className="space-y-3">
        <Breadcrumb repoName={repoParam ?? ''} />
        <Card>
          <CardContent>
            <SkeletonRows rows={8} />
          </CardContent>
        </Card>
      </div>
    );
  }

  if (err || !repo) {
    return (
      <div className="space-y-3">
        <Breadcrumb repoName={repoParam ?? ''} />
        <Card>
          <CardContent className="text-data text-destructive">
            {err || t('repoDetail.notFound')}
          </CardContent>
        </Card>
      </div>
    );
  }

  // repo.name is already the full "org/repo" pull reference (e.g. "acme/app").
  const pullBase = `${host}/${repo.name}`;
  // Use the most-recently-pushed tag as the exemplar; fall back to a placeholder.
  const latestTag = tags[0]?.tag;
  const exemplarPull = latestTag
    ? `docker pull ${pullBase}:${latestTag}`
    : `docker pull ${pullBase}:<tag>`;

  return (
    <div className="space-y-3">
      <Breadcrumb repoName={repo.name} />

      {/* ── Repo header ─────────────────────────────────────────────────────── */}
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="break-all text-display font-semibold text-slate-100">{repo.name}</h1>
            <VisibilityBadge visibility={repo.visibility} />
          </div>
          <p className="mt-0.5 text-data text-slate-400">
            <span className="tnum">{repo.tag_count}</span>{' '}
            {t('repoDetail.tagsUnit', { count: repo.tag_count })}
            {' · '}
            {/* Manifest size only — never present as image pull size */}
            <span className="tnum">{formatBytes(repo.size_bytes)}</span>{' '}
            {t('repoDetail.manifestUnit')}
            {' · '}
            {t('repoDetail.created')}{' '}
            <span className="tnum" title={formatUnix(toUnix(repo.created_at))}>
              {formatRelative(toUnix(repo.created_at))}
            </span>
          </p>
        </div>
        {/* Visibility toggle — admin/owner only */}
        {canAdminOrg && (
          <Button
            size="sm"
            variant="outline"
            disabled={busyVis}
            onClick={() => void onToggleVisibility()}
          >
            {repo.visibility === 'public'
              ? t('repoDetail.makePrivate')
              : t('repoDetail.makePublic')}
          </Button>
        )}
      </div>

      {/* ── docker pull command ─────────────────────────────────────────────── */}
      <Card>
        <CardHeader>
          <CardTitle>{t('repoDetail.pull.title')}</CardTitle>
          <span className="text-data text-slate-500">
            {repo.visibility === 'public'
              ? t('repoDetail.pull.public')
              : t('repoDetail.pull.private')}
          </span>
        </CardHeader>
        <CardContent className="flex items-center gap-2">
          <code className="tnum flex-1 break-all rounded border border-slate-800 bg-slate-950 px-3 py-2 text-data text-slate-200">
            {exemplarPull}
          </code>
          <CopyButton text={exemplarPull} />
        </CardContent>
      </Card>

      {/* ── Tags table ──────────────────────────────────────────────────────── */}
      <Card>
        <CardHeader>
          <CardTitle>{t('repoDetail.tags.title')}</CardTitle>
          <span className="tnum text-data text-slate-500">{repo.tag_count}</span>
        </CardHeader>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('repoDetail.col.tag')}</TableHead>
              <TableHead className="w-44">{t('repoDetail.col.digest')}</TableHead>
              {/* "Manifest size" — not "layer size" or "image size" */}
              <TableHead className="w-28 text-right">{t('repoDetail.col.manifestSize')}</TableHead>
              {/* arch is always empty today — column stays honest */}
              <TableHead className="w-12 text-right">{t('repoDetail.col.arch')}</TableHead>
              <TableHead className="w-28 text-right">{t('repoDetail.col.pushed')}</TableHead>
              <TableHead className="w-20 text-right">{t('repoDetail.col.pull')}</TableHead>
              {canAdminOrg && (
                <TableHead className="w-20 text-right">{t('repoDetail.col.delete')}</TableHead>
              )}
            </TableRow>
          </TableHeader>
          <TableBody>
            {tags.length === 0 ? (
              <EmptyRow colSpan={canAdminOrg ? 7 : 6}>{t('repoDetail.tags.empty')}</EmptyRow>
            ) : (
              tags.map((t) => {
                const tagPullCmd = `docker pull ${pullBase}:${t.tag}`;
                const pushedUnix = toUnix(t.pushed_at);
                return (
                  <TableRow key={t.tag}>
                    <TableCell className="font-medium text-slate-100">{t.tag}</TableCell>
                    <TableCell
                      className="tnum text-slate-400"
                      title={t.digest || undefined}
                    >
                      {shortDigest(t.digest)}
                    </TableCell>
                    <TableCell className="tnum text-right text-slate-400">
                      {formatBytes(t.size)}
                    </TableCell>
                    {/* arch — always "—", per honesty contract */}
                    <TableCell className="tnum text-right text-slate-500">—</TableCell>
                    <TableCell
                      className="tnum text-right text-slate-400"
                      title={formatUnix(pushedUnix)}
                    >
                      {formatRelative(pushedUnix)}
                    </TableCell>
                    <TableCell className="text-right">
                      <CopyButton text={tagPullCmd} />
                    </TableCell>
                    {canAdminOrg && (
                      <TableCell className="text-right">
                        <DeleteTagDialog
                          tag={t.tag}
                          busy={busyTag === t.tag}
                          onConfirm={() => void onDeleteTag(t.tag)}
                        />
                      </TableCell>
                    )}
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </Card>
    </div>
  );
}

function Breadcrumb({ repoName }: { repoName: string }) {
  const { t } = useTranslation();
  return (
    <nav
      className="flex items-center gap-1.5 text-data text-slate-400"
      aria-label={t('repoDetail.breadcrumbAria')}
    >
      <Link
        to="/repos"
        className="rounded transition-colors duration-fast hover:text-slate-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
      >
        {t('repos.title')}
      </Link>
      <span aria-hidden className="text-slate-700">
        /
      </span>
      <span className="text-slate-200">{repoName}</span>
    </nav>
  );
}

/**
 * DeleteTagDialog — a guarded confirmation before removing a tag pointer.
 *
 * The copy emphasises that only the tag reference is removed: the manifest and
 * layer blobs remain in the CAS and are cleaned up by GC. This prevents the
 * operator from thinking they are reclaiming disk space immediately.
 */
function DeleteTagDialog({
  tag,
  busy,
  onConfirm,
}: {
  tag: string;
  busy: boolean;
  onConfirm: () => void;
}) {
  const [open, setOpen] = useState(false);
  const { t } = useTranslation();
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" variant="destructive" disabled={busy}>
          {t('common.delete')}
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t('repoDetail.deleteTag.title')}</DialogTitle>
          <DialogDescription>
            {/* The tag name rides in as a component, not an interpolated value:
                Trans parses the interpolated string as HTML. */}
            <Trans
              i18nKey="repoDetail.deleteTag.description"
              components={{ tag: <span className="font-medium text-slate-200">{tag}</span> }}
            />
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <DialogClose asChild>
            <Button size="sm" variant="outline">
              {t('common.cancel')}
            </Button>
          </DialogClose>
          <Button
            size="sm"
            variant="destructive"
            onClick={() => {
              onConfirm();
              setOpen(false);
            }}
          >
            {t('repoDetail.deleteTag.confirm')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
