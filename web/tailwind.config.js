/** @type {import('tailwindcss').Config} */
// ─────────────────────────────────────────────────────────────────────────────
// Specula WebUI theme — "industrial instrument panel".
//
// Committed direction (REGISTRY-DESIGN §5.0), not a shadcn default:
//   · single typeface: IBM Plex Mono (self-hosted via @fontsource), sans == mono
//   · one accent only: instrument amber #ffb02e — buttons, active nav, focus ring
//   · neutrals: `slate` REMAPPED to a warm near-black ramp (not Tailwind slate)
//     950 app bg · 900 panel · 800 border/input · 700 strong divider
//     400 secondary text · 100 primary text (warm white, never pure #fff)
//   · radius 2–3px (near-square), hairline 1px dividers instead of shadows
//   · dense by default: this is an operator tool, density is a feature
//
// shadcn/ui is the BEHAVIOUR base (Radix a11y/keyboard/focus). Its visuals are
// fully re-skinned here — the CSS variables below are wired to our ramp in
// src/index.css, so a copied-in shadcn component inherits our look, not theirs.
//
// STATUS COLOUR IS A SEPARATE AXIS FROM THE ACCENT. Amber is the only accent
// (interactive/brand). Trust tiers and upstream health are *information*, and
// REGISTRY-DESIGN requires them to read semantically — they use the instrument
// lamp ramp below (`tier-*`, `health-*`) and are never rendered as solid amber,
// so a status badge can never be mistaken for an interactive control.
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  darkMode: ['class'],
  theme: {
    extend: {
      colors: {
        // ── instrument amber: the ONLY accent ────────────────────────────────
        brand: {
          DEFAULT: '#ffb02e',
          fg: '#1a1200', // text/icon colour on an amber fill
        },

        // ── warm near-black neutral ramp (Tailwind `slate` is overridden) ────
        slate: {
          50: '#f6f3ee',
          100: '#ece7dd', // primary text
          200: '#d3ccc0',
          300: '#b0a696',
          400: '#8c8477', // secondary text
          500: '#6b6459',
          600: '#4a443b',
          700: '#332e27', // strong divider / secondary button border
          800: '#1c1a16', // hairline border / input
          900: '#131210', // panel surface
          950: '#0a0908', // app background
        },

        // ── trust tiers (PRD §G2) ────────────────────────────────────────────
        // Ordinal: checksum < tofu < consensus < signed. Encoded as a quality
        // ladder (danger → warn → info → ok) so "how well was this verified"
        // reads at a glance without consulting a legend.
        //
        // `tofu` is lemon (hue 52, high sat) rather than amber (hue 37): it must
        // never be confused with the accent. Never fill a tier badge solid —
        // use the tinted/outlined treatment (see .badge-tier-* in index.css).
        tier: {
          signed: 'hsl(var(--status-ok))', // cryptographic trust root
          consensus: 'hsl(var(--status-info))', // N-mirror quorum
          tofu: 'hsl(var(--status-warn))', // first-use lock only
          checksum: 'hsl(var(--status-danger))', // transport integrity only
        },

        // ── upstream health (REGISTRY-DESIGN §5.3) ───────────────────────────
        // `unknown` is deliberately the neutral secondary-text colour: an
        // un-probed mirror is an absence of data, not a good state.
        health: {
          up: 'hsl(var(--status-ok))',
          blocked: 'hsl(var(--status-danger))',
          probing: 'hsl(var(--status-info))',
          unknown: 'hsl(var(--status-unknown))',
        },

        // ── shadcn/ui contract, wired to OUR ramp (see index.css) ────────────
        border: 'hsl(var(--border))',
        input: 'hsl(var(--input))',
        ring: 'hsl(var(--ring))',
        background: 'hsl(var(--background))',
        foreground: 'hsl(var(--foreground))',
        primary: {
          DEFAULT: 'hsl(var(--primary))',
          foreground: 'hsl(var(--primary-foreground))',
        },
        secondary: {
          DEFAULT: 'hsl(var(--secondary))',
          foreground: 'hsl(var(--secondary-foreground))',
        },
        destructive: {
          DEFAULT: 'hsl(var(--destructive))',
          foreground: 'hsl(var(--destructive-foreground))',
        },
        muted: {
          DEFAULT: 'hsl(var(--muted))',
          foreground: 'hsl(var(--muted-foreground))',
        },
        accent: {
          DEFAULT: 'hsl(var(--accent))',
          foreground: 'hsl(var(--accent-foreground))',
        },
        popover: {
          DEFAULT: 'hsl(var(--popover))',
          foreground: 'hsl(var(--popover-foreground))',
        },
        card: {
          DEFAULT: 'hsl(var(--card))',
          foreground: 'hsl(var(--card-foreground))',
        },
      },

      // One typeface for everything. `sans` intentionally resolves to the same
      // mono stack: the instrument-panel voice depends on it.
      //
      // CJK STRATEGY — font fallback is resolved PER GLYPH, which is what makes
      // this work. IBM Plex Mono has no han glyphs, so a Chinese character
      // falls through to the next family that does, while every Latin
      // character in the same sentence still resolves to Plex Mono. Mixed
      // "push 镜像到 registry" lines therefore keep Latin in the identity face
      // instead of dragging the whole run into a CJK font.
      //
      // Order is the whole argument:
      //   1. IBM Plex Mono        — the identity. Owns all Latin/digits/symbols.
      //   2. Noto Sans SC Subset  — OUR chosen CJK face, self-hosted (index.css).
      //                             Covers GB2312 L1 + all UI copy → ~everything.
      //   3. PingFang/YaHei/…     — the honest tail. A rare han glyph outside
      //                             the subset (an unusual name in user data)
      //                             still renders in a sane CJK face rather
      //                             than tofu boxes. Only these rare glyphs are
      //                             ever OS-dependent — never the UI chrome.
      //   4. ui-monospace/…       — pre-existing Latin mono fallbacks.
      fontFamily: {
        sans: [
          '"IBM Plex Mono"',
          '"Noto Sans SC Subset"',
          '"PingFang SC"',
          '"Hiragino Sans GB"',
          '"Microsoft YaHei"',
          '"Noto Sans CJK SC"',
          '"Source Han Sans SC"',
          'ui-monospace',
          'SFMono-Regular',
          'Menlo',
          'Consolas',
          'monospace',
        ],
        mono: [
          '"IBM Plex Mono"',
          '"Noto Sans SC Subset"',
          '"PingFang SC"',
          '"Hiragino Sans GB"',
          '"Microsoft YaHei"',
          '"Noto Sans CJK SC"',
          '"Source Han Sans SC"',
          'ui-monospace',
          'SFMono-Regular',
          'Menlo',
          'Consolas',
          'monospace',
        ],
      },

      // Near-square. shadcn components ask for lg/md/sm off --radius; all three
      // land at 2–3px so nothing rounds itself back into a generic template.
      borderRadius: {
        lg: 'var(--radius)',
        md: 'calc(var(--radius) - 1px)',
        sm: 'calc(var(--radius) - 1px)',
        DEFAULT: 'var(--radius)',
      },

      // Dense, deliberate type scale. The jump from `data` to `readout` is the
      // hierarchy: there is no soup of intermediate sizes.
      fontSize: {
        micro: ['10px', { lineHeight: '14px', letterSpacing: '0.06em' }],
        label: ['11px', { lineHeight: '15px', letterSpacing: '0.04em' }],
        data: ['12.5px', { lineHeight: '18px' }],
        body: ['13.5px', { lineHeight: '20px' }],
        section: ['15px', { lineHeight: '22px', letterSpacing: '-0.01em' }],
        display: ['22px', { lineHeight: '28px', letterSpacing: '-0.02em' }],
        readout: ['30px', { lineHeight: '34px', letterSpacing: '-0.03em' }],
      },

      // Restrained motion: reveal hierarchy, nothing decorative.
      transitionDuration: {
        fast: '90ms',
        DEFAULT: '140ms',
      },
      transitionTimingFunction: {
        instrument: 'cubic-bezier(0.16, 1, 0.3, 1)',
      },
      keyframes: {
        'panel-in': {
          from: { opacity: '0', transform: 'translateY(3px)' },
          to: { opacity: '1', transform: 'translateY(0)' },
        },
        'overlay-in': {
          from: { opacity: '0' },
          to: { opacity: '1' },
        },
        // A blocked mirror's lamp: the one deliberate "memorable moment".
        'lamp-pulse': {
          '0%, 100%': { opacity: '1' },
          '50%': { opacity: '0.35' },
        },
      },
      animation: {
        'panel-in': 'panel-in 140ms cubic-bezier(0.16, 1, 0.3, 1)',
        'overlay-in': 'overlay-in 90ms linear',
        'lamp-pulse': 'lamp-pulse 1.8s ease-in-out infinite',
      },
    },
  },
  plugins: [require('tailwindcss-animate')],
};
