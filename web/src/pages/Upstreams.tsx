import { useCallback, useEffect, useRef, useState } from 'react';
import { GripVertical } from 'lucide-react';

import {
  getUpstreams,
  patchUpstream,
  reorderUpstreams,
  unblockUpstream,
} from '@/api/client';
import type { ProtocolUpstreams, UpstreamHealth } from '@/api/types';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';
import { HealthBadge } from '@/components/ui/badge';
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
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { useToast } from '@/hooks/use-toast';
import { cn, formatPercent, formatRelative } from '@/lib/utils';

/**
 * Upstreams — the per-protocol mirror chain ops view (REGISTRY-DESIGN §5.3).
 *
 * This page is the DESIGN REFERENCE for the ops UI zone: semantic health colour,
 * honest "—" for unmeasured data, hairline density, text-colour-as-state tabs,
 * one amber action per row.
 *
 * R3 extension: drag-to-reorder the fallback chain. Each row gains a GripVertical
 * drag handle. Dropping calls POST /admin/upstreams/{protocol}/reorder with the
 * full new mirror name order. An optimistic local preview is shown immediately and
 * reverted if the server rejects the change.
 *
 * Owned by: the Ops UI agent.
 */
export function Upstreams() {
  const [protocols, setProtocols] = useState<ProtocolUpstreams[]>([]);
  const [err, setErr] = useState('');
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const { toast } = useToast();

  const load = useCallback(() => {
    getUpstreams()
      .then((r) => setProtocols(r.protocols ?? []))
      .catch((e: unknown) => setErr(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, []);

  useEffect(load, [load]);

  /** replace swaps one protocol's chain in place from a mutation's response. */
  const replace = (updated: ProtocolUpstreams) =>
    setProtocols((prev) =>
      prev.map((p) => (p.protocol === updated.protocol ? updated : p))
    );

  const onToggle = async (protocol: string, m: UpstreamHealth) => {
    const key = `${protocol}/${m.name}`;
    setBusy(key);
    try {
      replace(await patchUpstream(protocol, m.name, { enabled: !m.enabled }));
      toast({
        variant: 'success',
        title: m.enabled ? 'Mirror disabled' : 'Mirror enabled',
        description: `${protocol} · ${m.name}`,
      });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: 'Could not update mirror',
        description: e instanceof Error ? e.message : String(e),
        duration: Infinity,
      });
    } finally {
      setBusy(null);
    }
  };

  const onUnblock = async (protocol: string, m: UpstreamHealth) => {
    const key = `${protocol}/${m.name}`;
    setBusy(key);
    try {
      replace(await unblockUpstream(protocol, m.name));
      toast({
        variant: 'success',
        title: 'Mirror unblocked',
        description: `${protocol} · ${m.name}`,
      });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: 'Could not unblock mirror',
        description: e instanceof Error ? e.message : String(e),
        duration: Infinity,
      });
    } finally {
      setBusy(null);
    }
  };

  /**
   * onReorder is called by ChainPanel after a successful drag-drop.
   * It sends the new order to the server and updates the chain in place.
   * Throws on failure so ChainPanel can revert the local preview.
   */
  const onReorder = async (protocol: string, newOrder: string[]) => {
    setBusy(`${protocol}/reorder`);
    try {
      replace(await reorderUpstreams(protocol, { order: newOrder }));
      toast({
        variant: 'success',
        title: 'Mirror order saved',
        description: `${protocol} · ${newOrder.join(' → ')}`,
      });
    } catch (e: unknown) {
      toast({
        variant: 'destructive',
        title: 'Reorder failed',
        description: e instanceof Error ? e.message : String(e),
        duration: Infinity,
      });
      throw e; // ChainPanel catches this to revert preview
    } finally {
      setBusy(null);
    }
  };

  if (loading) {
    return (
      <div className="space-y-3">
        <PageHeading />
        <Card>
          <CardContent>
            <SkeletonRows rows={8} />
          </CardContent>
        </Card>
      </div>
    );
  }

  if (err) {
    return (
      <div className="space-y-3">
        <PageHeading />
        <Card>
          <CardContent className="text-data text-destructive">{err}</CardContent>
        </Card>
      </div>
    );
  }

  if (protocols.length === 0) {
    return (
      <div className="space-y-3">
        <PageHeading />
        <Card>
          <CardContent className="text-data text-slate-400">
            No protocols configured.
          </CardContent>
        </Card>
      </div>
    );
  }

  return (
    <div className="space-y-3">
      <PageHeading />
      <Tabs defaultValue={protocols[0].protocol}>
        <TabsList>
          {protocols.map((p) => {
            const down = p.mirrors.filter((m) => m.blocked).length;
            return (
              <TabsTrigger key={p.protocol} value={p.protocol}>
                {p.protocol}
                {/* A blocked-mirror count is the one thing worth surfacing on the
                    tab itself — an operator must see trouble without opening it. */}
                {down > 0 && (
                  <span className="tnum rounded-[2px] bg-health-blocked/15 px-1 text-micro font-semibold text-health-blocked">
                    {down}
                  </span>
                )}
              </TabsTrigger>
            );
          })}
        </TabsList>

        {protocols.map((p) => (
          <TabsContent key={p.protocol} value={p.protocol}>
            <ChainPanel
              chain={p}
              busy={busy}
              onToggle={(m) => void onToggle(p.protocol, m)}
              onUnblock={(m) => void onUnblock(p.protocol, m)}
              onReorder={(order) => onReorder(p.protocol, order)}
            />
          </TabsContent>
        ))}
      </Tabs>
    </div>
  );
}

function PageHeading() {
  return (
    <div>
      <h1 className="text-display font-semibold text-slate-100">Upstreams</h1>
      <p className="mt-0.5 text-data text-slate-400">
        Per-protocol fallback chain — order, health, and who is actually serving
        misses. Drag the{' '}
        <span className="inline-flex items-center gap-0.5 text-slate-300">
          <GripVertical className="size-3 inline-block" aria-hidden />
          handle
        </span>{' '}
        to reorder.
      </p>
    </div>
  );
}

// ── ChainPanel ─────────────────────────────────────────────────────────────────

interface ChainPanelProps {
  chain: ProtocolUpstreams;
  busy: string | null;
  onToggle: (m: UpstreamHealth) => void;
  onUnblock: (m: UpstreamHealth) => void;
  /** Called with the full ordered name list after a successful drag-drop. Throws on failure. */
  onReorder: (newOrder: string[]) => Promise<void>;
}

function ChainPanel({ chain, busy, onToggle, onUnblock, onReorder }: ChainPanelProps) {
  /**
   * Drag-reorder state.
   *
   * `previewMirrors` is a local optimistic reorder applied immediately on drop
   * and cleared once the server responds (or on error to revert).
   * `displayMirrors` is what renders: preview when dragging, chain.mirrors otherwise.
   */
  const [dragIdx, setDragIdx] = useState<number | null>(null);
  const [overIdx, setOverIdx] = useState<number | null>(null);
  const [previewMirrors, setPreviewMirrors] = useState<UpstreamHealth[] | null>(null);
  const [reordering, setReordering] = useState(false);
  const dragCounter = useRef(0); // tracks nested dragenter/dragleave on the tbody

  // When chain.mirrors changes (server confirm or parent update), drop any preview.
  useEffect(() => {
    setPreviewMirrors(null);
  }, [chain.mirrors]);

  const displayMirrors = previewMirrors ?? chain.mirrors;

  // A row is blocked from actions during any pending op for its mirror.
  const isReorderBusy = busy === `${chain.protocol}/reorder` || reordering;

  const handleDragStart = (e: React.DragEvent<HTMLTableRowElement>, idx: number) => {
    setDragIdx(idx);
    e.dataTransfer.effectAllowed = 'move';
    // The default ghost image is the row — that is fine.
    // Give the cursor a frame to paint before we dim the source.
    requestAnimationFrame(() => {
      setDragIdx(idx); // re-set to trigger repaint
    });
  };

  const handleDragOver = (e: React.DragEvent<HTMLTableRowElement>, idx: number) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    if (overIdx !== idx) setOverIdx(idx);
  };

  const handleDragEnd = () => {
    setDragIdx(null);
    setOverIdx(null);
    dragCounter.current = 0;
  };

  const handleDrop = async (e: React.DragEvent<HTMLTableRowElement>, toIdx: number) => {
    e.preventDefault();
    const fromIdx = dragIdx;
    setDragIdx(null);
    setOverIdx(null);
    dragCounter.current = 0;

    if (fromIdx === null || fromIdx === toIdx) return;

    // Build the new order array.
    const newMirrors = [...displayMirrors];
    const [moved] = newMirrors.splice(fromIdx, 1);
    newMirrors.splice(toIdx, 0, moved);

    // Optimistic local preview.
    setPreviewMirrors(newMirrors);
    setReordering(true);

    try {
      await onReorder(newMirrors.map((m) => m.name));
      // Success: parent called replace(); chain.mirrors is now the server truth.
      // The useEffect above will clear previewMirrors on next render.
    } catch {
      // Revert: drop the preview, chain.mirrors is still the old order.
      setPreviewMirrors(null);
    } finally {
      setReordering(false);
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>{chain.protocol} fallback chain</CardTitle>
        <div className="flex items-center gap-3 text-data text-slate-400">
          {/* When the chain is not instrumented, say so plainly rather than
              rendering zeros that would look like measurements. */}
          {!chain.live ? (
            <span className="text-health-unknown">
              config only · not instrumented
            </span>
          ) : (
            <>
              <span>
                served{' '}
                <span className="tnum text-slate-200">{chain.total_served}</span>
              </span>
              <span className="text-slate-700">·</span>
              <span>
                last by{' '}
                <span className="text-slate-200">
                  {chain.last_served_by || '—'}
                </span>
              </span>
            </>
          )}
          {reordering && (
            <span className="text-brand animate-pulse">saving order…</span>
          )}
        </div>
      </CardHeader>

      <Table>
        <TableHeader>
          <TableRow>
            {/* Grip + order number share the first column. */}
            <TableHead className="w-10 text-right" aria-label="Drag handle / order" />
            <TableHead className="w-40">Mirror</TableHead>
            <TableHead>URL</TableHead>
            <TableHead className="w-28">Health</TableHead>
            <TableHead className="w-20 text-right">Latency</TableHead>
            <TableHead className="w-24 text-right">Hit share</TableHead>
            <TableHead className="w-24 text-right">Last served</TableHead>
            <TableHead className="w-36 text-right">Actions</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {displayMirrors.length === 0 ? (
            <EmptyRow colSpan={8}>
              No mirrors configured for {chain.protocol}.
            </EmptyRow>
          ) : (
            displayMirrors.map((m, idx) => {
              const key = `${chain.protocol}/${m.name}`;
              const isBusy = busy === key || isReorderBusy;
              const isDragging = dragIdx === idx;
              const isDropTarget = overIdx === idx && dragIdx !== null && dragIdx !== idx;

              return (
                <TableRow
                  key={m.name}
                  draggable
                  onDragStart={(e) => handleDragStart(e, idx)}
                  onDragOver={(e) => handleDragOver(e, idx)}
                  onDragEnd={handleDragEnd}
                  onDrop={(e) => { void handleDrop(e, idx); }}
                  className={cn(
                    !m.enabled && 'opacity-45',
                    isDragging && 'opacity-25'
                  )}
                  style={
                    isDropTarget
                      ? { boxShadow: 'inset 0 2px 0 -1px #ffb02e' }
                      : undefined
                  }
                >
                  {/* Drag handle + order number */}
                  <TableCell className="w-10 pr-1">
                    <div className="flex items-center justify-end gap-1">
                      <span className="tnum text-right text-slate-500 text-data">
                        {m.order}
                      </span>
                      <span
                        className="cursor-grab text-slate-600 transition-colors duration-fast hover:text-slate-300 active:cursor-grabbing"
                        aria-label="Drag to reorder"
                        title="Drag to reorder this mirror in the fallback chain"
                      >
                        <GripVertical className="size-3.5" aria-hidden />
                      </span>
                    </div>
                  </TableCell>

                  <TableCell>
                    <div className="flex items-center gap-1.5">
                      <span className="font-medium text-slate-100">{m.name}</span>
                      {m.official && (
                        <span
                          className="text-micro uppercase tracking-wider text-slate-500"
                          title="Configured as the authoritative origin."
                        >
                          origin
                        </span>
                      )}
                      {m.overridden && (
                        <span
                          className="text-micro uppercase tracking-wider text-brand"
                          title={`Reordered at runtime. YAML baseline priority: ${m.config_priority}.`}
                        >
                          override
                        </span>
                      )}
                    </div>
                  </TableCell>

                  <TableCell
                    className="max-w-[1px] truncate text-slate-400"
                    title={m.url}
                  >
                    {m.url}
                  </TableCell>

                  <TableCell>
                    <div className="flex items-center gap-1.5">
                      <HealthBadge health={m.health} />
                      {m.last_err && (
                        <span
                          className="truncate text-micro text-slate-500"
                          title={m.last_err}
                        >
                          {m.last_err}
                        </span>
                      )}
                    </div>
                  </TableCell>

                  {/* Unmeasured values render "—". Never a fabricated 0. */}
                  <TableCell className="tnum text-right text-slate-300">
                    {m.has_latency ? `${m.last_latency_ms} ms` : '—'}
                  </TableCell>
                  <TableCell className="tnum text-right text-slate-300">
                    {chain.total_served > 0 ? formatPercent(m.hit_share) : '—'}
                  </TableCell>
                  <TableCell className="tnum text-right text-slate-400">
                    {formatRelative(m.last_served_unix)}
                  </TableCell>

                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      {m.blocked && (
                        <Button
                          size="sm"
                          variant="default"
                          disabled={isBusy}
                          onClick={() => onUnblock(m)}
                        >
                          Unblock
                        </Button>
                      )}
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={isBusy}
                        onClick={() => onToggle(m)}
                      >
                        {m.enabled ? 'Disable' : 'Enable'}
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              );
            })
          )}
        </TableBody>
      </Table>
    </Card>
  );
}
