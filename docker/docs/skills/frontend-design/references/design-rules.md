# Hard design rules

> Consolidated from design-engineering writeups. These are non-negotiable defaults — break them only with deliberate rationale.

## Typography

- **Cap body line length at 65-75ch.** Wider and the eye loses the line return; narrower and the rag fights you.
- **Hierarchy through scale + weight contrast (ratio ≥ 1.25 between steps).** Flat scales read as no decision made.
- **Cap font-family count at 3** (display + body + optional mono). More reads as indecision. One well-tuned family with weight contrast usually beats three competing typefaces.
- **Don't pair similar-but-not-identical fonts** (two geometric sans-serifs, two humanist sans-serifs). Pair on a contrast axis: serif + sans, geometric + humanist, OR use one family in multiple weights.
- **Hero / display heading ceiling: clamp() max ≤ 6rem (~96px).** Above that the page is shouting, not designing.
- **Display letter-spacing floor: ≥ -0.04em.** Tighter and letters touch; reads as cramped, not designed. -0.02 to -0.03em is plenty for tight grotesques.
- **No all-caps body copy.** Reserve uppercase for short labels (≤4 words) and badges.
- **Use `text-wrap: balance` on h1-h3** for even line lengths; **`text-wrap: pretty` on long prose** to reduce orphans.
- **Body text leading 1.4-1.6** (Tailwind: `leading-relaxed` or `leading-6`). Cramped body text is the single most visible "AI built this" tell after font choice.
- **Never go below 14px (`text-sm`) for content the user has to read.**
- **Proper typographic characters: smart quotes, real em/en dashes** (rendered, not the literal char) where they belong, no double spaces. Straight quotes signal amateur typography.

## Color & contrast

- **Verify contrast.** Body text must hit ≥4.5:1 against its background; large text (≥18px or bold ≥14px) needs ≥3:1. Placeholder text needs the same 4.5:1, not the muted-gray default. The single biggest reason AI designs feel hard to read is muted gray body text on a tinted near-white.
- **Use OKLCH for all color tokens.** Predictable lightness across hues; predictable mixing.
- **Tinted neutrals: add 0.005-0.015 chroma toward the brand's hue.** Don't default-tint toward warm or cool "because the brand feels that way" — that's the cross-project monoculture move.
- **Pick a color strategy before picking colors.** Four steps on the commitment axis:
  - **Restrained**: tinted neutrals + one accent ≤10%. Product default; brand minimalism.
  - **Committed**: one saturated color carries 30-60% of the surface. Brand default for identity-driven pages.
  - **Full palette**: 3-4 named roles, each used deliberately. Brand campaigns; product data viz.
  - **Drenched**: the surface IS the color. Brand heroes, campaign pages.
- **Dark vs. light is never a default.** Before choosing, write one sentence of physical scene: who uses this, where, under what ambient light, in what mood. If the sentence doesn't force the answer, add detail until it does.
- **No information conveyed by color alone** (~10% of males are colorblind). Add redundant cues: shape, text, position.
- **Shadows use hue-shifted colors**, not pure black. Warm subjects → warm shadows shifted toward red/orange; cool subjects → cool shadows shifted toward blue.
- **Functional colors follow conventions**: red = error, green = success, blue = link, amber = warning.

## Layout & spacing

- **Vary spacing for rhythm.** Same spacing everywhere = no grouping hierarchy.
- **Cards are the lazy answer.** Use only when they're truly the best affordance. Nested cards are always wrong.
- **Flex for 1D, Grid for 2D.** Don't default to Grid when `flex-wrap` would be simpler.
- **For responsive grids without breakpoints**: `repeat(auto-fit, minmax(280px, 1fr))`.
- **Build a semantic z-index scale** (dropdown → sticky → modal-backdrop → modal → toast → tooltip). Never arbitrary values like 999 or 9999.
- **Tailwind spacing scale ONLY**: `p-4 mx-2 py-6` — never arbitrary values like `p-[16px] mx-[8px] py-[24px]`. Arbitrary values defeat the design system and ship as inconsistent numbers across components.
- **Never mix `space-*` with `gap-*`** on the same element. Pick one. `gap-*` on flex/grid containers is the default.
- **Mobile-first.** Build small-screen first, add `md:` / `lg:` second. Most "AI built this on a 27-inch monitor" complaints trace to never loading mobile.
- **Touch targets minimum 44×44px** (iOS), 48×48dp (Android). Expand hit area when the icon is smaller via `hitSlop` / padding.
- **Test heading copy at every breakpoint.** Long heading words + large clamp scales + narrow grids cause headline overflow on tablet/mobile. The viewport is part of the design.

## Motion

- **Motion is intentional, not an afterthought.** Consider it part of the build.
- **Don't animate CSS layout properties** (width/height/top/left) unless truly needed. Animate `transform` and `opacity`.
- **Ease out with exponential curves**: `ease-out-quart`, `ease-out-quint`, `ease-out-expo`. **No bounce, no elastic.**
- **Reduced motion is not optional.** Every animation needs a `@media (prefers-reduced-motion: reduce)` alternative — typically a crossfade or instant transition.
- **Timing**: 100ms for micro-interactions, 200-300ms for standard transitions, 400-500ms for complex orchestrations.
- **Use libraries for advanced motion** (Motion, GSAP, anime.js, Lenis). Don't reinvent.
- **Staggering items within ONE list is legitimate.** The tell is the uniform reflex (one identical entrance applied to every section), not motion itself.
- **Reveal animations must enhance an already-visible default.** Don't gate content visibility on a class-triggered transition — transitions pause on hidden tabs and headless renderers, so the reveal never fires and the section ships blank.
- **Premium motion materials**: blur, backdrop-filter, clip-path, mask, shadow/glow are part of the palette when they materially improve the effect and stay smooth.

## Interaction

- **Dropdowns rendered with `position: absolute` inside an `overflow: hidden` or `overflow: auto` container will be clipped.** Use the native `<dialog>` / popover API, `position: fixed`, or a portal to escape the stacking context.
- **Every interactive element has all states**: default, hover, focus, active, disabled, loading, error, success. Forgetting any one is a partial implementation.
- **Use `:focus-visible`** for keyboard focus rings, not `:focus`. Mouse clicks don't trigger a focus ring; keyboard navigation does.
- **Stable interaction states**: use color, opacity, or elevation transitions for press states without changing layout bounds. Layout-shifting transforms that move surrounding content trigger jitter.
- **Tap feedback within 80-150ms** (ripple/opacity/elevation). No visual response on tap = perceived as broken.

## Copy

- **Every word earns its place.** No restated headings, no intros that repeat the title.
- **Button labels: verb + object.** "Save changes" beats "OK"; "Delete project" beats "Yes". The label says what will happen.
- **Link text needs standalone meaning.** "View pricing plans" beats "Click here"; screen readers announce links out of context.
- **No em dashes (`—`) or `--`.** Use commas, colons, semicolons, periods, or parentheses.
- **No marketing buzzwords**: streamline, empower, supercharge, leverage, unleash, transform, seamless, world-class, enterprise-grade, next-generation, cutting-edge, game-changer, mission-critical.
- **No aphoristic cadence** as the page's recurring voice ("Serious statement. Then a punchy short negation."). If three or more section copy blocks land on a short rebuttal-shaped sentence, rewrite.

## Polish (these are part of "done", not optional)

- **Every async surface needs a loading skeleton or spinner.** A blank box while data loads = bug report.
- **Every list needs an empty state** with one short sentence ("No projects yet — create one →") and an action if the user can fix it.
- **Every form needs validation**: required fields, format checks, server-error display. Silent failure on submit = uninstall.
- **Toasts (sonner or shadcn toast) for success/error on every user-initiated action.** Never let an API call "succeed quietly" — the user can't tell if anything happened.
- **No emojis as icons.** Use lucide-react (or Phosphor for native). Emojis render differently per OS/browser and look like placeholder UI.
- **Use shadcn/ui primitives** (button, card, input, dialog, …) — never hand-roll the ones already in `components/ui/`. Customize via class overrides.
- **Stroke and style consistency on icons.** One stroke width per visual layer (1.5px or 2px). One style per hierarchy level (filled OR outline, not mixed).

## Final rule

Ship something **interesting rather than boring**, but **never ugly**.
