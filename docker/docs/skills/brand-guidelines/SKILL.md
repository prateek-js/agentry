---
name: brand-guidelines
description: Apply consistent brand colors, typography, and voice across every page or artifact in a project. Use when the user has an existing brand to honor (provided as tokens, a style guide, or by pointing at an existing design), or when establishing a brand from scratch for a new project. Covers brand color tokens, typography pairings, voice and tone, and asset usage.
license: Apache 2.0
---

# Brand consistency

The job of this skill is to keep what you build *feel like it came from one place*. When the operator or user has defined a brand — colors, fonts, voice — every page, component, and email must read as part of the same system.

## Three modes

### 1. Apply an existing brand

If the project has any of the following, read them BEFORE writing any visual code:

- `tailwind.config.{js,ts}` → brand tokens (colors, fonts) live in `theme.extend`
- `src/styles/globals.css` or `src/styles/tokens.css` → CSS custom properties
- `DESIGN.md`, `BRAND.md`, `tokens.json` at repo root → designer hand-off
- `components/ui/` from shadcn → already-tuned primitives; use them, don't rebuild
- `public/logo.svg` or `public/brand/` → official assets

Reuse the tokens verbatim. Do NOT invent new colors. Do NOT replace the brand font with your favorite. Identity preservation wins over personal aesthetic.

### 2. Establish a new brand (no prior assets)

If no brand tokens exist, build one before scaffolding any pages. The brand is the design system; pages are instances of it.

1. **Choose a color strategy** from `../frontend-design/references/design-rules.md` (Restrained / Committed / Full palette / Drenched).
2. **Pick a primary brand color in OKLCH** based on mood and audience. Avoid the saturated AI defaults (cyan-on-dark, purple-to-blue gradients, cream/sand bodies).
3. **Compose the rest of the palette** around it: bg, surface, ink, accent, muted. Tinted neutrals get 0.005-0.015 chroma toward the brand hue.
4. **Pick typography**: one display family + one body family (+ optional mono). Pair on a contrast axis (serif + sans, geometric + humanist). Avoid Inter, Roboto, Open Sans as the primary.
5. **Pick a voice**: 2-3 adjectives ("warm, precise, direct" — not "professional and modern"). Every copy block must read in that voice.
6. **Commit to disk**: write the brand to `src/styles/tokens.css` (CSS vars), `tailwind.config.ts` (theme.extend), and a short `BRAND.md` at repo root that records the why so future turns don't drift.

### 3. Honor an operator-provided brand

If the operator has staged brand assets at `/etc/sandbox/creds/brand/` or elsewhere under `/etc/sandbox/`, discover them on first turn with `ls -la /etc/sandbox/`. Treat operator-provided tokens as ground truth — same rule as mode 1.

## Brand voice across pages

Even when the visual brand is locked, voice drifts page-to-page if you don't anchor it:

- **One copy reviewer at the end of every build.** Re-read every heading, button, microcopy, and empty state with the brand voice adjectives in mind. Rewrite anything that doesn't read in voice.
- **No restated headings.** Hero says X; the next section can't restate X.
- **No marketing buzzwords** (streamline, empower, supercharge, leverage, unleash, transform, seamless, world-class, enterprise-grade). Use specific nouns and verbs that describe what the product does literally.
- **Button labels: verb + object** ("Save changes", "Delete project") — never "OK" or "Submit".
- **Link text: standalone meaning** ("View pricing plans" not "Click here").

## Asset usage

- **Logo**: use the official SVG. Don't recolor, don't squish proportions, don't redraw. Maintain clear space (logo's height of padding around it, minimum).
- **Icons**: one family per project. Lucide (web) or Phosphor (native) by default. Match the stroke width across all icons in the same visual layer (1.5px or 2px). Match filled vs outline by hierarchy level (don't mix at the same level).
- **Images**: respect aspect ratio. Don't stretch. Don't auto-saturate.

## Cross-page consistency check

Before declaring a multi-page project done:

- [ ] Every page uses the same primary CTA color and the same primary button shape
- [ ] Every page uses the same display + body font pairing
- [ ] Every page uses the same spacing rhythm (one scale, not "page A uses 8px and page B uses 12px")
- [ ] Every page's heading hierarchy uses the same type scale
- [ ] Every page's tone matches the 2-3 voice adjectives — not "polished" on the home page and "casual" on the pricing page

If any check fails, the project is inconsistent — fix before shipping.
