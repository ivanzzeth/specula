import * as React from 'react';
import { cva, type VariantProps } from 'class-variance-authority';

import { cn } from '@/lib/utils';

/**
 * Badge — the semantic status primitive.
 *
 * REGISTRY-DESIGN §5.0 requires trust tiers and upstream health to read BY
 * COLOUR meaningfully, not decoratively. That is what the `tier` and `health`
 * variants below are for; they are the only sanctioned way to render those
 * values, so the encoding stays identical across every view.
 *
 * Treatment: tinted background + hairline border + coloured text. Never a solid
 * fill — solid amber is the interactive accent, and a status badge must never
 * be mistaken for a button.
 *
 * Trust tiers are ORDINAL (checksum < tofu < consensus < signed) and are mapped
 * onto a quality ladder: danger → warn → info → ok. "signed" being green and
 * "checksum" being red is the whole point — checksum alone is NOT a
 * supply-chain control, and the UI must say so at a glance.
 */
const badgeVariants = cva(
  cn(
    'inline-flex items-center gap-1 rounded-[2px] border px-1.5 py-0.5',
    'text-micro font-semibold uppercase tracking-wider whitespace-nowrap'
  ),
  {
    variants: {
      variant: {
        default: 'border-slate-700 bg-slate-800 text-slate-300',
        outline: 'border-slate-700 bg-transparent text-slate-400',
        accent: 'border-brand/40 bg-brand/10 text-brand',

        // ── trust tiers (PRD §G2) ────────────────────────────────────────────
        'tier-signed': 'border-tier-signed/40 bg-tier-signed/10 text-tier-signed',
        'tier-consensus': 'border-tier-consensus/40 bg-tier-consensus/10 text-tier-consensus',
        'tier-tofu': 'border-tier-tofu/40 bg-tier-tofu/10 text-tier-tofu',
        'tier-checksum': 'border-tier-checksum/40 bg-tier-checksum/10 text-tier-checksum',

        // ── upstream health (REGISTRY-DESIGN §5.3) ───────────────────────────
        'health-up': 'border-health-up/40 bg-health-up/10 text-health-up',
        'health-blocked': 'border-health-blocked/40 bg-health-blocked/10 text-health-blocked',
        'health-probing': 'border-health-probing/40 bg-health-probing/10 text-health-probing',
        // "unknown" is intentionally colourless: no data is not a good state.
        'health-unknown': 'border-slate-700 bg-transparent text-health-unknown',

        // ── repo visibility ──────────────────────────────────────────────────
        // private is the safe default and stays neutral; public is the one that
        // warrants attention, because it means anonymous pull is allowed.
        private: 'border-slate-700 bg-slate-800 text-slate-300',
        public: 'border-health-probing/40 bg-health-probing/10 text-health-probing',
      },
    },
    defaultVariants: { variant: 'default' },
  }
);

export interface BadgeProps
  extends React.HTMLAttributes<HTMLSpanElement>,
    VariantProps<typeof badgeVariants> {}

function Badge({ className, variant, ...props }: BadgeProps) {
  return <span className={cn(badgeVariants({ variant }), className)} {...props} />;
}

/** The four verification tiers, exactly as the API's `tier` field spells them. */
export type Tier = 'signed' | 'consensus' | 'tofu' | 'checksum';

/** The four upstream health states, exactly as the API's `health` field spells them. */
export type Health = 'up' | 'blocked' | 'probing' | 'unknown';

/**
 * TierBadge renders a verification tier with its sanctioned colour.
 *
 * An unrecognised tier renders neutral rather than guessing a colour: claiming
 * a trust level we do not understand would be worse than saying nothing.
 */
export function TierBadge({ tier, className }: { tier: string; className?: string }) {
  const known: Record<Tier, VariantProps<typeof badgeVariants>['variant']> = {
    signed: 'tier-signed',
    consensus: 'tier-consensus',
    tofu: 'tier-tofu',
    checksum: 'tier-checksum',
  };
  const variant = known[tier as Tier] ?? 'outline';
  return (
    <Badge variant={variant} className={className} title={tierHint(tier)}>
      {tier || 'unknown'}
    </Badge>
  );
}

/** tierHint is the honest one-line explanation shown on hover. */
function tierHint(tier: string): string {
  switch (tier) {
    case 'signed':
      return 'Anchored in a cryptographic trust root obtained out-of-band. Highest tier.';
    case 'consensus':
      return 'Digest agreed by multiple independent mirrors. Quorum, not authenticity.';
    case 'tofu':
      return 'Digest locked on first fetch; changes since then would alert. No trust root.';
    case 'checksum':
      return 'Transport integrity only, against a value that may come from the mirror itself. Not a supply-chain control.';
    default:
      return 'Unknown verification tier.';
  }
}

/**
 * HealthBadge renders an upstream mirror's health with a status lamp.
 *
 * A blocked mirror pulses — the one deliberate motion accent in the ops view,
 * because "an upstream is down" is the single thing an operator must not miss.
 */
export function HealthBadge({ health, className }: { health: string; className?: string }) {
  const known: Record<Health, VariantProps<typeof badgeVariants>['variant']> = {
    up: 'health-up',
    blocked: 'health-blocked',
    probing: 'health-probing',
    unknown: 'health-unknown',
  };
  const variant = known[health as Health] ?? 'health-unknown';
  const lamp: Record<Health, string> = {
    up: 'bg-health-up',
    blocked: 'bg-health-blocked animate-lamp-pulse',
    probing: 'bg-health-probing',
    unknown: 'bg-health-unknown',
  };
  return (
    <Badge variant={variant} className={className} title={healthHint(health)}>
      <span className={cn('lamp', lamp[health as Health] ?? 'bg-health-unknown')} />
      {health || 'unknown'}
    </Badge>
  );
}

/** healthHint is the honest one-line explanation shown on hover. */
function healthHint(health: string): string {
  switch (health) {
    case 'up':
      return 'Last request succeeded; not blocked.';
    case 'blocked':
      return 'Auto-block tripped after consecutive failures. This mirror is being skipped.';
    case 'probing':
      return 'Recent failures, but still below the auto-block threshold; still being tried.';
    default:
      return 'Not contacted since this instance started — no data. Not a claim of health.';
  }
}

/** VisibilityBadge renders a hosted repo's private/public visibility. */
export function VisibilityBadge({
  visibility,
  className,
}: {
  visibility: string;
  className?: string;
}) {
  const isPublic = visibility === 'public';
  return (
    <Badge
      variant={isPublic ? 'public' : 'private'}
      className={className}
      title={
        isPublic
          ? 'Anyone, including anonymous clients, may pull this repo.'
          : 'Only org members may pull this repo.'
      }
    >
      {visibility || 'private'}
    </Badge>
  );
}

export { Badge, badgeVariants };
