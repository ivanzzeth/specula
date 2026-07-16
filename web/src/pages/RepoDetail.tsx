import { useCallback, useEffect, useState } from 'react';
import { Link, useParams } from 'react-router-dom';

import { deleteRepoTag, getRepo, listRepoTags, patchRepo } from '@/api/client';
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
import { formatBytes, formatRelative, formatUnix } from '@/lib/utils';

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
  return (
    <Button
      size="sm"
      variant="outline"
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
      {done ? '✓' : 'Copy'}
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
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : String(e)))
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
      toast({ variant: 'success', title: 'Tag deleted', description: tag });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: 'Could not delete tag',
        description: e instanceof Error ? e.message : String(e),
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
        title: next === 'public' ? 'Repository is now public' : 'Repository is now private',
        description:
          next === 'public'
            ? 'Anonymous pull is now allowed.'
            : 'Only org members may pull.',
      });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: 'Could not update visibility',
        description: e instanceof Error ? e.message : String(e),
        duration: Infinity,
      });
    } finally {
      setBusyVis(false);
    }
  };

  if (!activeOrg) {
    return (
      <Card>
        <CardContent className="text-data text-slate-400">
          No active organisation selected.
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
            {err || 'Repository not found.'}
          </CardContent>
        </Card>
      </div>
    );
  }

  const host = window.location.host;
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
            {repo.tag_count === 1 ? 'tag' : 'tags'}
            {' · '}
            {/* Manifest size only — never present as image pull size */}
            <span className="tnum">{formatBytes(repo.size_bytes)}</span> manifest
            {' · '}
            created{' '}
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
            {repo.visibility === 'public' ? 'Make private' : 'Make public'}
          </Button>
        )}
      </div>

      {/* ── docker pull command ─────────────────────────────────────────────── */}
      <Card>
        <CardHeader>
          <CardTitle>Pull</CardTitle>
          <span className="text-data text-slate-500">
            {repo.visibility === 'public'
              ? 'Public — anonymous pull allowed'
              : 'Private — docker login required'}
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
          <CardTitle>Tags</CardTitle>
          <span className="tnum text-data text-slate-500">{repo.tag_count}</span>
        </CardHeader>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Tag</TableHead>
              <TableHead className="w-44">Digest</TableHead>
              {/* "Manifest size" — not "layer size" or "image size" */}
              <TableHead className="w-28 text-right">Manifest size</TableHead>
              {/* arch is always empty today — column stays honest */}
              <TableHead className="w-12 text-right">Arch</TableHead>
              <TableHead className="w-28 text-right">Pushed</TableHead>
              <TableHead className="w-20 text-right">Pull</TableHead>
              {canAdminOrg && <TableHead className="w-20 text-right">Delete</TableHead>}
            </TableRow>
          </TableHeader>
          <TableBody>
            {tags.length === 0 ? (
              <EmptyRow colSpan={canAdminOrg ? 7 : 6}>
                No tags yet. Push an image to this repository to create the first tag.
              </EmptyRow>
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
  return (
    <nav className="flex items-center gap-1.5 text-data text-slate-400" aria-label="Breadcrumb">
      <Link
        to="/repos"
        className="rounded transition-colors duration-fast hover:text-slate-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
      >
        Repositories
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
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm" variant="destructive" disabled={busy}>
          Delete
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete tag</DialogTitle>
          <DialogDescription>
            Remove the tag pointer{' '}
            <span className="font-medium text-slate-200">{tag}</span>? The manifest and layers
            remain in the content-addressable store and are cleaned up by the garbage
            collector on its next eviction cycle.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <DialogClose asChild>
            <Button size="sm" variant="outline">
              Cancel
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
            Delete tag
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
