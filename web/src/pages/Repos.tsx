import { useCallback, useEffect, useMemo, useState } from 'react';
import { Trans, useTranslation } from 'react-i18next';
import { Link, useNavigate } from 'react-router-dom';

import { ApiError, listRepos } from '@/api/client';
import type { RepoDTO } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Card, CardContent } from '@/components/ui/card';
import { VisibilityBadge } from '@/components/ui/badge';
import { Input } from '@/components/ui/input';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';
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
import { useOrg } from '@/components/org-context';
import { translateServerError } from '@/i18n/server-errors';
import { formatBytes, formatRelative } from '@/lib/utils';
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
 * Extract the bare repo name from the full "org/repo" string.
 * e.g. "acme/app" with orgSlug "acme" → "app".
 */
function bareRepo(name: string, orgSlug: string): string {
  const prefix = `${orgSlug}/`;
  return name.startsWith(prefix) ? name.slice(prefix.length) : name;
}

/**
 * Repositories — hosted repos in the active org (REGISTRY-DESIGN §5.1).
 *
 * Empty state teaches the user to push rather than just saying "nothing here".
 * The filter is local: the server returns all repos for the org and we slice
 * client-side, which is appropriate when a developer org has tens, not millions.
 *
 * Owned by: Agent 1 · Registry.
 */
export function Repos() {
  const { activeOrg } = useOrg();
  const navigate = useNavigate();
  const { t } = useTranslation();

  const [repos, setRepos] = useState<RepoDTO[]>([]);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(true);
  const [search, setSearch] = useState('');
  const [visFilter, setVisFilter] = useState<'all' | 'public' | 'private'>('all');

  const load = useCallback(() => {
    const org = activeOrg;
    if (!org) return;
    setLoading(true);
    setErr('');
    listRepos(org.slug)
      .then((r) => setRepos(r.repos ?? []))
      .catch((e: unknown) => setErr(errText(e)))
      .finally(() => setLoading(false));
  }, [activeOrg]);

  useEffect(load, [load]);

  const filtered = useMemo(
    () =>
      repos.filter((r) => {
        const nameMatch = !search || r.name.toLowerCase().includes(search.toLowerCase());
        const visMatch = visFilter === 'all' || r.visibility === visFilter;
        return nameMatch && visMatch;
      }),
    [repos, search, visFilter]
  );

  if (!activeOrg) {
    return (
      <div className="space-y-3">
        <PageHeading orgSlug="" />
        <Card>
          <CardContent className="text-data text-slate-400">
            {t('repos.noActiveOrg')}
          </CardContent>
        </Card>
      </div>
    );
  }

  if (loading) {
    return (
      <div className="space-y-3">
        <PageHeading orgSlug={activeOrg.slug} />
        <Card>
          <CardContent>
            <SkeletonRows rows={6} />
          </CardContent>
        </Card>
      </div>
    );
  }

  if (err) {
    return (
      <div className="space-y-3">
        <PageHeading orgSlug={activeOrg.slug} />
        <Card>
          <CardContent className="text-data text-destructive">{err}</CardContent>
        </Card>
      </div>
    );
  }

  const hasFilter = Boolean(search) || visFilter !== 'all';

  return (
    <div className="space-y-3">
      {/* Header + push guide shortcut */}
      <div className="flex items-end justify-between gap-3">
        <PageHeading orgSlug={activeOrg.slug} />
        <Button variant="default" size="sm" asChild>
          <Link to="/push">{t('repos.pushGuide')}</Link>
        </Button>
      </div>

      {/* Filter bar — only shown when there is something to filter */}
      {repos.length > 0 && (
        <div className="flex items-center gap-2">
          <Input
            className="w-56"
            placeholder={t('repos.filterPlaceholder')}
            aria-label={t('repos.filterPlaceholder')}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          <Select
            value={visFilter}
            onValueChange={(v) => setVisFilter(v as 'all' | 'public' | 'private')}
          >
            <SelectTrigger className="w-32">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">{t('common.all')}</SelectItem>
              <SelectItem value="private">{t('visibility.private')}</SelectItem>
              <SelectItem value="public">{t('visibility.public')}</SelectItem>
            </SelectContent>
          </Select>
          {hasFilter && (
            <button
              onClick={() => {
                setSearch('');
                setVisFilter('all');
              }}
              className="rounded text-data text-slate-400 transition-colors duration-fast hover:text-slate-100 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
            >
              {t('repos.clear')}
            </button>
          )}
          <span className="ml-auto tnum text-data text-slate-500" title={t('repos.countTitle')}>
            {filtered.length} / {repos.length}
          </span>
        </div>
      )}

      <Card>
        {repos.length === 0 ? (
          <EmptyStateContent orgSlug={activeOrg.slug} />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t('repos.col.repository')}</TableHead>
                <TableHead className="w-24">{t('repos.col.visibility')}</TableHead>
                {/* "Manifest size" — not "image size", per honesty contract */}
                <TableHead className="w-32 text-right">{t('repos.col.manifestSize')}</TableHead>
                <TableHead className="w-16 text-right">{t('repos.col.tags')}</TableHead>
                <TableHead className="w-28 text-right">{t('repos.col.lastPush')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.length === 0 ? (
                <EmptyRow colSpan={5}>{t('repos.noMatch')}</EmptyRow>
              ) : (
                filtered.map((r) => {
                  const bare = bareRepo(r.name, activeOrg.slug);
                  return (
                    <TableRow
                      key={r.id}
                      className="cursor-pointer"
                      tabIndex={0}
                      role="link"
                      aria-label={t('repos.rowAria', { name: r.name })}
                      onClick={() => navigate(`/repos/${encodeURIComponent(bare)}`)}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter' || e.key === ' ') {
                          e.preventDefault();
                          navigate(`/repos/${encodeURIComponent(bare)}`);
                        }
                      }}
                    >
                      <TableCell>
                        {/* Full "org/repo" name — the pull reference after the host */}
                        <span className="font-medium text-slate-100">{r.name}</span>
                      </TableCell>
                      <TableCell>
                        <VisibilityBadge visibility={r.visibility} />
                      </TableCell>
                      <TableCell className="tnum text-right text-slate-400">
                        {formatBytes(r.size_bytes)}
                      </TableCell>
                      <TableCell className="tnum text-right text-slate-300">
                        {r.tag_count}
                      </TableCell>
                      <TableCell className="tnum text-right text-slate-400">
                        {formatRelative(toUnix(r.last_pushed_at))}
                      </TableCell>
                    </TableRow>
                  );
                })
              )}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  );
}

function PageHeading({ orgSlug }: { orgSlug: string }) {
  const { t } = useTranslation();
  return (
    <div>
      <h1 className="text-display font-semibold text-slate-100">{t('repos.title')}</h1>
      {orgSlug && (
        <p className="mt-0.5 text-data text-slate-400">
          {/* Trans keeps the org slug emphasised inside a sentence whose word
              order differs between English and Chinese. The slug rides in as a
              component rather than an interpolated value: Trans parses the
              interpolated string as HTML, so any value containing '<' (e.g. the
              '<org>' placeholder) would be swallowed as a tag. */}
          <Trans
            i18nKey="repos.subtitle"
            components={{ org: <span className="text-slate-200">{orgSlug}</span> }}
          />
        </p>
      )}
    </div>
  );
}

/**
 * EmptyStateContent — the "friction killer" for new orgs.
 *
 * Rather than a generic "nothing here" message, we show a ready-to-copy push
 * command and a prominent link to the full guide. The goal: an operator with
 * zero repos should know exactly what to do without leaving the page.
 */
function EmptyStateContent({ orgSlug }: { orgSlug: string }) {
  const host = useRegistryHost();
  const { t } = useTranslation();
  return (
    <div className="flex flex-col items-center gap-4 px-4 py-12 text-center">
      <div>
        <p className="text-section font-semibold text-slate-100">{t('repos.empty.title')}</p>
        <p className="mt-1 max-w-sm text-data text-slate-400">{t('repos.empty.body')}</p>
      </div>

      {/* Sample push command — the one thing an operator needs to get started */}
      <div className="w-full max-w-md rounded border border-slate-800 bg-slate-950 px-3 py-2.5 text-left text-data">
        <span className="text-slate-600">$</span>{' '}
        <span className="text-slate-200">
          docker push {host}/{orgSlug}/
        </span>
        <span className="text-brand">&lt;repo&gt;</span>
        <span className="text-slate-200">:&lt;tag&gt;</span>
      </div>

      <Button variant="default" size="sm" asChild>
        <Link to="/push">{t('repos.empty.cta')}</Link>
      </Button>
    </div>
  );
}
