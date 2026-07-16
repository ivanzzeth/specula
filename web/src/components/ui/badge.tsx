import * as React from 'react';
import { cva, type VariantProps } from 'class-variance-authority';
import { useTranslation } from 'react-i18next';

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
  cn('inline-flex items-center gap-1 rounded-[2px] border px-1.5 py-0.5', 'text-micro font-semibold whitespace-nowrap'),
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
    VariantProps<typeof badgeVariants> {
  /**
   * The badge's text is a guaranteed-Latin API literal (`tofu`, `up`, `public`)
   * that stays English in EVERY locale — so the caps device is safe and STAYS ON
   * in Chinese.
   *
   * Why this opt-out exists: `.label-caps` turns caps+tracking off under
   * `html[lang^='zh']`, which is right for a badge carrying translated copy
   * (`settings.restartPendingBadge` → "待重启" — tracking would prise its em grid
   * apart). But applying it to a status badge made the SAME literal render
   * "TOFU" in English and "tofu" in Chinese. REGISTRY-DESIGN §5.0 makes the
   * trust-tier/health encoding load-bearing, so it must read identically in both
   * languages. There is no CJK inside these badges to protect.
   *
   * Latin badges therefore use the plain utilities: with `.label-caps` absent,
   * the `html[lang^='zh']` rule cannot match them and cannot switch them off.
   */
  latin?: boolean;
}

function Badge({ className, variant, latin, ...props }: BadgeProps) {
  return (
    <span
      className={cn(
        badgeVariants({ variant }),
        latin ? 'uppercase tracking-wider' : 'label-caps',
        className
      )}
      {...props}
    />
  );
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
 *
 * ── WHY THE VALUE STAYS ENGLISH IN CHINESE ───────────────────────────────────
 * `signed`/`consensus`/`tofu`/`checksum` are the API's `tier` field literals.
 * They appear verbatim in API responses, logs and docs, and TOFU is an English
 * acronym of art that Chinese developers say in English. Translating the badge
 * would break the link between what the UI shows and what an operator greps —
 * the same reason digest/manifest/tag stay English. The TOOLTIP is the legend,
 * and that IS fully translated.
 */
export function TierBadge({ tier, className }: { tier: string; className?: string }) {
  const { t } = useTranslation();
  const known: Record<Tier, VariantProps<typeof badgeVariants>['variant']> = {
    signed: 'tier-signed',
    consensus: 'tier-consensus',
    tofu: 'tier-tofu',
    checksum: 'tier-checksum',
  };
  const variant = known[tier as Tier] ?? 'outline';
  const hint = known[tier as Tier]
    ? t(`tier.hint.${tier}`)
    : t('tier.hint.unknown');
  return (
    <Badge latin variant={variant} className={className} title={hint}>
      {tier || t('tier.unknown')}
    </Badge>
  );
}

/**
 * HealthBadge renders an upstream mirror's health with a status lamp.
 *
 * A blocked mirror pulses — the one deliberate motion accent in the ops view,
 * because "an upstream is down" is the single thing an operator must not miss.
 */
export function HealthBadge({ health, className }: { health: string; className?: string }) {
  const { t } = useTranslation();
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
  // Same rule as TierBadge: the value is the API's `health` literal and stays
  // English; the tooltip legend is translated.
  const hint = known[health as Health] ? t(`health.hint.${health}`) : t('health.hint.unknown');
  return (
    <Badge latin variant={variant} className={className} title={hint}>
      <span className={cn('lamp', lamp[health as Health] ?? 'bg-health-unknown')} />
      {health || t('health.unknown')}
    </Badge>
  );
}

/** VisibilityBadge renders a hosted repo's private/public visibility. */
export function VisibilityBadge({
  visibility,
  className,
}: {
  visibility: string;
  className?: string;
}) {
  const { t } = useTranslation();
  const isPublic = visibility === 'public';
  return (
    <Badge
      latin
      variant={isPublic ? 'public' : 'private'}
      className={className}
      title={isPublic ? t('visibility.hint.public') : t('visibility.hint.private')}
    >
      {visibility || t('visibility.private')}
    </Badge>
  );
}

export { Badge, badgeVariants };
