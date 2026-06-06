# AI Tells — concrete patterns to refuse

> Consolidated from public design-engineering writeups. Reference this whenever the user says "looks AI-generated", "looks generic", "looks like every other site", or you're auditing your own output before declaring done.

The problem with AI-generated UI isn't quality — it's **convergence**. Every model reaches for the same defaults, so those defaults become a recognizable signal of laziness. The fix is intentional choices traceable to purpose, audience, and medium.

---

## The AI slop test

After any UI build, ask: **"If someone said an AI made this, would they believe it immediately?"** If yes, identify which tells are present and rebuild the offending parts.

Run the test at two altitudes:
1. **First-order**: can someone guess the theme + palette from the *category* alone? (Fintech → navy + gold; AI tool → cyan-on-dark.) That's training-data reflex. Reframe.
2. **Second-order**: can someone guess from *category + anti-references* ("AI tool that's NOT SaaS-cream → editorial-typographic")? The first reflex was avoided; the second wasn't. Keep going.

---

## Typography tells

- [ ] **Inter, Roboto, Open Sans, system-ui as primary font.** Excellent fonts; that's the problem — they're the safe default every model reaches for. Pick a typeface with personality matched to the medium and mood.
- [ ] **Monospace as lazy shorthand for "technical".** Use mono for *data*, not for the marketing copy.
- [ ] **Large rounded-corner icon above every heading.** The "feature card" template.
- [ ] **All text centered.** Avoids the harder decision of asymmetric layout. Body text is left-aligned by default in real designs.
- [ ] **Display heading > 6rem (~96px) with letter-spacing tighter than -0.04em.** Above 6rem reads as shouting; tighter than -0.04em makes letters touch. -0.02 to -0.03em is plenty for tight grotesques.
- [ ] **All-caps body copy.** Reserve uppercase for labels ≤4 words and badges. Sentences in ALL CAPS are unreadable.
- [ ] **Tiny tracked uppercase eyebrow above EVERY section** ("ABOUT" "PROCESS" "PRICING"). One named kicker as a brand system is voice; an eyebrow on every section is AI grammar.
- [ ] **Numbered section markers (01 · About / 02 · Process / 03 · Pricing) when the sections aren't actually a sequence.** Numbers earn their place when the order carries information.

## Color tells

- [ ] **Cyan/teal on dark background.** The "AI dashboard" palette.
- [ ] **Purple-to-blue gradients.** The "AI product" gradient.
- [ ] **Neon accents on dark backgrounds.** Looks "cool" without color decisions.
- [ ] **Gradient text on headings or metrics** via `background-clip: text`. Decorative, never meaningful. Use a single solid color.
- [ ] **Pure #000 / pure #fff used throughout.** Real designs use tinted neutrals — add 0.005–0.015 chroma toward the brand hue.
- [ ] **Cream / sand / beige body backgrounds** (OKLCH L 0.84-0.97, C < 0.06, hue 40-100). Token names like `--paper`, `--cream`, `--sand`, `--ivory`, `--bone`, `--linen` are tells by themselves. This is the 2026 saturated default — "warm editorial" briefs all converge here. Pick a saturated brand color as the body, a true off-white at chroma 0, or a brand-tinted mid-tone instead.
- [ ] **Gray text on a colored background.** Looks washed out. Use a darker shade of the background's own hue, or a transparency of the text color.

## Layout tells

- [ ] **Everything wrapped in cards with identical padding + rounded corners.** Cards are the lazy answer. Use only when truly the best affordance.
- [ ] **Cards nested inside cards.** Always wrong.
- [ ] **Identical card grid: icon + heading + 2-line description × 3-6.** No dominance, no hierarchy.
- [ ] **Hero section with big number / small label / supporting stats.** The "metric dashboard" template.
- [ ] **Side-stripe borders** (>1px `border-left` accent on cards, list items, callouts). Never intentional. Use full borders, background tints, or nothing.
- [ ] **Same spacing everywhere.** No rhythm, no grouping. White space is the most powerful hierarchy signal — tight within groups, generous between.
- [ ] **`border-radius: 32px+` on cards / sections / inputs.** Cards top out at 12-16px; pill is fine for tags/buttons.
- [ ] **`border: 1px solid X` + `box-shadow` ≥16px blur on the same element.** The "ghost-card" pattern. Pick one.

## Motion tells

- [ ] **Bounce or elastic easing.** Feels 2015, not 2026. Ease-out with exponential curves (ease-out-quart / quint / expo).
- [ ] **Everything fades in from below with the same timing.** The uniform reflex. Each reveal should fit what it reveals.
- [ ] **Reveal animations that gate content visibility on a class-triggered transition.** Pauses on hidden tabs and headless renderers — section ships blank in screenshots.
- [ ] **Hover effects on everything with no hierarchy of importance.**
- [ ] **`transform` on `:hover` of an `<img>` element.** The image isn't an action target; animating it reads as "AI animated this because it could". Animate the card's background, border, or shadow instead.

## Detail tells

- [ ] **Glassmorphism as default** (blur, glass cards, glow borders) used decoratively. Rare and purposeful, or nothing.
- [ ] **Drop shadows that are all identical.** No depth hierarchy.
- [ ] **Sparklines as decoration.** Tiny charts that convey nothing.
- [ ] **`repeating-linear-gradient` diagonal stripe backgrounds** in `body:before` or section backgrounds. Pure decoration.
- [ ] **Hand-drawn / sketchy SVG illustrations** (`feTurbulence` "paper grain" filters, 5-30 crude paths meant to depict a tangible subject). Reads as amateurish. Use real assets or ship none.
- [ ] **Dark mode by default with glowing accents.** Avoids real color decisions. Choose light or dark based on content and audience.

## Copy tells

- [ ] **Em dashes.** Use commas, colons, semicolons, periods, or parentheses. No `--` either.
- [ ] **Marketing buzzwords**: streamline, empower, supercharge, leverage, unleash, transform, seamless, world-class, enterprise-grade, next-generation, cutting-edge, game-changer, mission-critical. Pick a specific noun and a verb that says what the product literally does.
- [ ] **Aphoristic-cadence body copy** as the page's recurring voice ("serious statement, then punchy short negation"). If three or more sections land on a short rebuttal-shaped sentence, rewrite.
- [ ] **"X theater" / "actually X" / "not just X, it's Y"** copy ("productivity theater", "growth theater"). Instant slop.
- [ ] **Button labels like "OK" / "Yes" / "Submit"**. Use verb + object: "Save changes", "Delete project".
- [ ] **Link text like "Click here" / "Learn more"** without standalone meaning. Screen readers announce links out of context.

---

## Absolute bans (match-and-refuse)

If you're about to write any of these, rewrite the element with different structure:

- Gradient text on headings/metrics
- Side-stripe borders >1px
- Glassmorphism used decoratively
- Hero-metric template (big number / small label / supporting stats)
- Identical card grids with no compositional hierarchy
- Tracked uppercase eyebrows above every section
- Numbered section markers when sections aren't a sequence
- Hand-drawn / sketchy SVG illustrations
- `repeating-linear-gradient` stripe backgrounds
- "X theater" copy phrases
- Text that overflows its container on tablet/mobile

---

## Transformation patterns

### Inter + card grid + gray palette → distinctive font + varied layout + intentional palette

Replace the default. Pick a typeface that matches the design's personality (geometric sans for precision, humanist serif for warmth, slab serif for authority). Replace the card grid with mixed presentation: a hero element, a list, a sidebar callout, a feature highlight with asymmetric layout. Build the palette from mood → base hue → color wheel relationships.

### Cyan-on-dark dashboard → purpose-driven palette grounded in audience

Start from the audience. A financial dashboard needs calm authority (deep navy + warm amber, not neon). A health app needs trust and warmth (soft greens + warm neutrals). Build the palette using color wheel schemes rooted in the psychological response the specific audience needs.

### Identical card grid → mixed presentation with hierarchy

Create a composition with clear dominance: one hero element that commands attention, supporting elements in varied formats (a list, a comparison table, a pull quote, a full-width callout). Use white space to create grouping — tight within related items, generous between groups.

### Centered everything → asymmetric layout with directional flow

Left-align body text. Use asymmetric layouts where a dominant element anchors one side and supporting content fills the other. Create clear directional flow guided by proportion and white space.

### Cream/sand body bg "for warm editorial" → saturated brand color OR true off-white

"Warmth" is carried by accent + typography + imagery, not by body bg. If the brief says "warm, traditional, magazine-restraint", pick (a) a saturated brand color as the body (terracotta, oxblood, deep ochre, near-black), (b) a true off-white at chroma 0, or (c) a darker mid-tone tinted toward the brand's own hue.

---

## Two-tier review

Run twice — once during build, once before declaring done.

**Severity ladder for findings:**

| Violation | Severity | Why |
|---|---|---|
| No aesthetic direction (defaults to "clean and modern") | Critical | Absence of design decisions |
| AI default font (Inter, Roboto, Open Sans) with no rationale | High | No typographic decision made |
| AI default palette (cyan-on-dark, purple-to-blue, cream/sand) with no rationale | High | No color theory applied |
| Identical card grid with no compositional hierarchy | High | No dominance, no flow |
| All text centered with no asymmetry | Medium | Avoidance of layout decisions |
| Uniform spacing throughout | Medium | No hierarchy through white space |
| Decorative effects (gradient text, sparklines, glassmorphism) without purpose | Low | Decoration masking absent decisions |
