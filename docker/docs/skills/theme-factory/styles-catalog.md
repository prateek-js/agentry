# Named style catalog

> A named library of distinct visual languages. Each spec is the minimum the model needs to render the style. Use when the user names a style directly ("build it in neobrutalist", "use dark cinema") or when you need a deliberate non-default direction.

The 10 themes under `themes/` give you tuned palettes for natural/minimal/tech contexts. This catalog gives you 50+ stronger aesthetic commitments — pick from here when "soft modern" or "tech innovation" isn't bold enough.

## How to use

User names a style → look it up here → implement the spec exactly. If the user describes a vibe without naming one, propose the 2-3 closest matches and let them pick.

Every style is implementable as inline CSS + Tailwind via CDN + Google Fonts + lucide-react / Font Awesome. No build step required.

---

## Minimal & clean

- **bento** — Apple/macOS bento grid. White bg, soft shadows, rounded `2xl`, Geist or SF Pro display, subtle accent color, generous gutters, varied-aspect tiles. Clean and modern without being cold.
- **soft modern** — White bg, blurred orb accents (purple/pink/blue at low opacity), rounded everything, Inter or Geist body, friendly and accessible. Use sparingly — close to AI default.
- **scandinavian** — Cold whites, extreme negative space, near-monochrome, light serif display (Söhne / Tiempos), no decoration. Hygge minimalism, quiet luxury.
- **corporate** — Conservative trust blues (#0a2540, #1a4378), structured 12-col grid, sans body (IBM Plex / Söhne), thin rules, no neon. B2B professionalism.
- **swiss** — Helvetica or Neue Haas Grotesk, rigid typographic grid, black + red only, zero decoration. Use red as the ONLY accent. Punctuation-as-design.
- **monolith** — White bg, dark navy/black shadows (sharp, no blur), thick top border accent, JetBrains Mono or Söhne Mono, brutalist restraint without the noise.

## Dark & atmospheric

- **dark cosmic** — Slate-950 bg, glowing indigo/cyan accents, radial dot grid background, glassmorphism cards, Inter display. AI tool default — use only when explicitly asked.
- **dark cinema** — Near-black (#0a0a0a), red glow accents (#dc2626 with `box-shadow` halo), Bebas Neue display, monospace body, noise overlay (`feTurbulence`), floating section labels. Atmospheric.
- **dark action** — Dark gradient bg, yellow/gold accents (#fbbf24), Oswald or Anton display, diagonal cuts, cinematic energy. Sports / film trailer.
- **dark mono** — Dark zinc-900 surfaces, cyan + pink accents (used sparingly), JetBrains Mono throughout, scanline texture via repeating gradient. Terminal aesthetic refined.
- **dark neon** — Black bg, multiple vivid neon colors (magenta + cyan + lime), bleed and bloom via large soft shadows. Use one color per region; don't overlap glows.
- **vaporwave** — Purple/teal gradients, retro grid floors (CSS perspective), synthwave glow, occasional glitch, VCR font for display. Use sparingly.

## Brutalist & bold

- **pure brutalist** — Monochrome black/white, hard 4px shadows offset bottom-right, monospace, no color at all, no rounding. Raw structure.
- **neobrutalist** — Hard 4-6px black shadows with vivid neon color accents (yellow, magenta, lime), thick 2-3px black borders, Archivo Black or Space Grotesk display. Playful and aggressive.
- **acid brutalist** — Pure black bg, acid yellow (#eeff00) + red (#ff0000) accents, Anton/Bebas Neue display, heavy noise grain. Underground zine energy.
- **utility terminal** — White bg, strict 1px borders, monospace throughout (JetBrains Mono / IBM Plex Mono), no rounding, grid texture background. Developer tool.

## Retro & nostalgic

- **retro terminal** — Green-on-black CRT (#0f0 on #000), phosphor glow via `text-shadow`, IBM Plex Mono / VT323, scanline overlay, blinking cursor. Vintage computing.
- **pixel** — 8-bit pixelated fonts (Press Start 2P), game UI conventions (HP bars, pixel borders), sprite-style icons, retro game palette (NES / Game Boy). Use full-screen, not as accent.
- **y2k** — Windows 95 beveled gray UI (#c0c0c0), MS Sans Serif / system, chunky pixel buttons, early-internet table layouts, blink-style decoration. Iconic, polarizing.
- **groovy** — Warm oranges/browns/mustard, 70s swirls and curves, rounded retro lettering (Sniglet / Stretch), psychedelic flow.
- **memphis** — 80s/90s geometric shapes (triangles, squiggles, dots), bright pastels on cream, confetti scatter, playful typography (geometric sans).
- **tropical** — Coral, turquoise, warm Miami palette, palm/wave decoration, vacation energy. Resort, hospitality, lifestyle.

## Artistic & expressive

- **pop art** — Cyan/pink/yellow on loud bg, floating bordered container, halftone dot patterns, comic-book speech bubbles, bold display.
- **kawaii** — Super cute pastels (mint, peach, lavender), bubble rounded forms, character illustration accents, soft drop shadows. Children's apps, hobby sites.
- **manga** — Speed lines, bold ink outlines (3-4px black), dramatic panel layouts, high contrast B&W with one accent color, bold display sans.
- **psychedelic** — Acid swirls, melting text (via `clip-path` distortion or SVG filters), rainbow gradients, mind-bending repeating patterns. Music, art.
- **zine** — Photocopied DIY aesthetic, cut-and-paste collage (rotated elements, taped corners), raw indie typography (mixed fonts intentionally), grain overlay.
- **aurora** — Flowing multi-color gradient backgrounds (mesh gradients), silk light effect, soft and dreamy. Beauty, wellness.

## Elegant & luxury

- **luxury** — Cream/off-white bg, serif display (Tiempos / Canela / Cardo), gold accents (#a07a00), generous whitespace, refined kerning. Spirits, fashion, hospitality.
- **art deco** — Geometric gold ornaments, strict symmetry, 1920s glamour, deep navy/black + gold, ornamental dividers. Limited runs only.
- **cottagecore** — Floral patterns, watercolor washes (low-opacity image fills), storybook serif (Cardo / Lora), botanical drawings.
- **gothic** — Dark greens (#1a2e1a) / blacks, ornate serif (UnifrakturMaguntia / Cormorant), candle-wax drips via SVG, moody atmosphere.
- **japanese** — Wabi-sabi imperfection, ink brush strokes, kanji-inspired negative space, asymmetric layouts, single accent color (vermilion or indigo).

## Technical & structured

- **blueprint** — Deep blueprint blue (#0a3656), white grid lines, Courier Prime / Iosevka, technical drawing aesthetic, callouts with leader lines.
- **dot grid** — Gray dotted bg (`radial-gradient`), Archivo Black + Space Mono, hot pink accent, hard 4px shadows.
- **pink neo** — Hot pink dotted bg, Archivo Black + Space Mono, pink/yellow/blue palette. Playful neobrutalist variant.
- **dashboard** — Chart-forward, dense metrics, sidebar nav, admin/analytics aesthetic. Use real chart libs (Recharts / D3), not decorative sparklines.
- **sci-fi hud** — Heads-up display, corner brackets (`clip-path: polygon`), data readouts, radar/targeting UI elements. Game, military aesthetic.
- **cyberpunk** — Yellow/black warning stripes, HUD overlays, neon on dark, danger aesthetics, Orbitron or Rajdhani display. Use restraint.

## Specialty & immersive

- **glassmorphism** — Frosted glass cards (`backdrop-filter: blur(20px)`) on gradient mesh bg, soft translucency. Default-AI-tell — only use when explicitly requested.
- **neumorphism** — Soft same-color shadows (one light from top-left, one dark from bottom-right) creating extruded soft UI on light gray (#e0e5ec). Looks dated; use deliberately.
- **clay** — Clay morphism, chunky rounded cards with physical depth (inner highlight + outer shadow), pastel palette, playful 3D-without-3D.
- **newspaper** — Black ink on newsprint (#f5f1e8), serif fonts (Old Style / Playfair), editorial column layouts, drop caps, rules between sections.
- **longform** — Full-bleed hero images, pull quotes, drop caps, rich magazine editorial flow, serif body (Charter / Tiempos Text). The Atlantic / Verge / Medium.
- **skeuomorphic** — Realistic material textures (leather, paper, brushed metal), depth and shadows mimicking physical objects, beveled buttons. iOS 6 era.
- **organic** — Earthy tones (terracotta, sage, ochre), rounded organic shapes (no straight edges), warm hand-crafted feel, humanist serif (Mercury / Tiempos Text).
- **handwritten** — Hand-drawn borders, pencil textures, imperfect sketch-like lines (Caveat / Patrick Hand), notebook-page bg. Education, kids.

## Energy & motion

- **athletic** — Diagonal cuts (`clip-path` slashes), bold color blocks, high-impact sport energy, Bebas Neue / Druk display, motion-emphasizing transitions. Fitness, sports, sneakers.
- **grunge** — Worn textures (PNG overlays), splatter marks, distressed rough torn edges, band-poster aesthetic, mixed display fonts.
- **isometric** — 3D isometric grid illustrations, flat-color depth via SVG, layered objects, axonometric perspective. Tech / SaaS hero illustrations.
- **maximalist** — Everything layered, dense pattern-on-pattern, opulent visual chaos. Only ship if you can sustain the density across every page — partial maximalism reads broken.
- **enterprise editorial** — White/dark alternating sections, indigo accent (#4f46e5), large rounded app cards with screenshot previews. Modern B2B (Linear / Vercel / Stripe-style).

---

## Picking a style

When the user asks "what style fits a fintech app?" or similar, propose 2-3 options on different axes:

- **Trust-first**: `corporate` or `enterprise editorial` — calm authority.
- **Distinctive**: `swiss` or `monolith` — typographic statement.
- **Modern boutique**: `bento` or `soft modern` — friendly accessibility.

Avoid proposing `dark cosmic`, `glassmorphism`, or `cream/sand` palettes as the first suggestion for any AI-adjacent product — they're the saturated AI defaults of 2026.

## Combining with themes/

The `themes/` directory holds 10 tuned palettes (`arctic-frost`, `botanical-garden`, etc.) for natural/minimal/tech contexts. You can layer:

- This catalog → aesthetic language (typography + composition + signature elements)
- `themes/` → tuned palette inside that language

E.g., "luxury style + botanical-garden palette" → serif display + generous whitespace + green/sage palette, rather than the default cream/gold.

## Output shape

Generate a single self-contained HTML file:
- Inline CSS (no external stylesheet)
- Tailwind via CDN (or vanilla CSS if Tailwind would fight the style)
- Google Fonts via CDN
- lucide-react or Font Awesome for icons
- Vanilla JS for interactions
- No build step
