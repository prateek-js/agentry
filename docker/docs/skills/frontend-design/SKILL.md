---
name: frontend-design
description: Create distinctive, production-grade frontend interfaces with high design quality. Use this skill when the user asks to build web components, pages, artifacts, posters, or applications (examples include websites, landing pages, dashboards, React components, HTML/CSS layouts, or when styling/beautifying any web UI). Generates creative, polished code and UI design that avoids generic AI aesthetics.
license: Apache 2.0
---

This skill guides creation of distinctive, production-grade frontend interfaces that avoid generic "AI slop" aesthetics. Implement real working code with exceptional attention to aesthetic details and creative choices.

The user provides frontend requirements: a component, page, application, or interface to build. They may include context about the purpose, audience, or technical constraints.

## Design thinking

Before coding, understand the context and commit to a BOLD aesthetic direction:
- **Purpose**: What problem does this interface solve? Who uses it?
- **Tone**: Pick an extreme — brutally minimal, maximalist chaos, retro-futuristic, organic/natural, luxury/refined, playful/toy-like, editorial/magazine, brutalist/raw, art deco/geometric, soft/pastel, industrial/utilitarian. See `../theme-factory/styles-catalog.md` for 50+ named directions you can pick from directly.
- **Constraints**: Technical requirements (framework, performance, accessibility).
- **Differentiation**: What makes this UNFORGETTABLE? What's the one thing someone will remember?

**CRITICAL**: Choose a clear conceptual direction and execute it with precision. Bold maximalism and refined minimalism both work — the key is intentionality, not intensity.

Then implement working code (HTML/CSS/JS, React, Vue, etc.) that is:
- Production-grade and functional
- Visually striking and memorable
- Cohesive with a clear aesthetic point-of-view
- Meticulously refined in every detail

## Frontend aesthetics guidelines

Focus on:
- **Typography**: Choose fonts that are beautiful, unique, and interesting. Avoid generic fonts like Arial and Inter; opt instead for distinctive choices that elevate the frontend's aesthetics; unexpected, characterful font choices. Pair a distinctive display font with a refined body font.
- **Color & theme**: Commit to a cohesive aesthetic. Use CSS variables for consistency. Dominant colors with sharp accents outperform timid, evenly-distributed palettes.
- **Motion**: Use animations for effects and micro-interactions. Prioritize CSS-only solutions for HTML. Use Motion library for React when available. Focus on high-impact moments: one well-orchestrated page load with staggered reveals (animation-delay) creates more delight than scattered micro-interactions. Use scroll-triggering and hover states that surprise.
- **Spatial composition**: Unexpected layouts. Asymmetry. Overlap. Diagonal flow. Grid-breaking elements. Generous negative space OR controlled density.
- **Backgrounds & visual details**: Create atmosphere and depth rather than defaulting to solid colors. Add contextual effects and textures that match the overall aesthetic. Apply creative forms like gradient meshes, noise textures, geometric patterns, layered transparencies, dramatic shadows, decorative borders, custom cursors, and grain overlays.

NEVER use generic AI-generated aesthetics like overused font families (Inter, Roboto, Arial, system fonts), cliched color schemes (particularly purple gradients on white backgrounds), predictable layouts and component patterns, and cookie-cutter design that lacks context-specific character.

Interpret creatively and make unexpected choices that feel genuinely designed for the context. No design should be the same. Vary between light and dark themes, different fonts, different aesthetics. NEVER converge on common choices (Space Grotesk, for example) across generations.

**IMPORTANT**: Match implementation complexity to the aesthetic vision. Maximalist designs need elaborate code with extensive animations and effects. Minimalist or refined designs need restraint, precision, and careful attention to spacing, typography, and subtle details. Elegance comes from executing the vision well.

## Deeper references (load on demand)

Lazy-load these when the situation calls for them — the SKILL.md above carries the philosophy; these files carry the rules and detection patterns:

- **`references/ai-tells.md`** — concrete AI-tell detection checklist, absolute bans (gradient text, side-stripe borders, identical card grids, eyebrows on every section, the cream/sand/beige body bg of 2026), and transformation patterns (default → intentional). Read this when reviewing an existing design, when the user says "looks AI-generated", or before declaring your own build done.
- **`references/design-rules.md`** — hard rules across typography (line length, scale ratio, font count, hero ceiling, letter-spacing floor), color (contrast minimums, OKLCH, color strategy commitment ladder), layout, motion, interaction, and copy. Reference when in doubt about a specific quantity.
- **`../theme-factory/styles-catalog.md`** — 50+ named visual languages (bento, neobrutalist, dark cinema, swiss, art deco, longform, …) with the minimum spec to render each. Use when the user names a style directly or when you need a stronger commitment than "soft modern".
- **`../theme-factory/themes/`** — 10 tuned palettes (arctic-frost, botanical-garden, midnight-galaxy, …) for natural/minimal/tech contexts.

## Workflow

1. **Before coding**: pick an aesthetic direction in 2-3 specific words. Reject "clean and modern" — that's the absence of direction. State the direction explicitly.
2. **During coding**: for every visual choice, ask "would a different AI make this same choice?" If yes, reconsider. Verify the choice is specific to this design's direction, not a generic default.
3. **Before declaring done**: run the AI slop test from `references/ai-tells.md`. If someone could believe AI made this without doubt, identify which tells are present and rebuild the offending parts.

Don't hold back. Show what can be created when committing fully to a distinctive vision.
