# Hopper – Brand Guidelines

## Identity

**Name:** Hopper
**Tagline:** Privacy-first download manager for power users
**Tone:** Technical, minimal, direct. No hand-holding, no marketing fluff.

---

## Logo

- Wordmark: `hopper` — lowercase, monospace or semi-bold sans
- Optional icon: Känguru-Silhouette mit Downloadpfeil (TBD)
- In UI-Topbar: `hopper` + Version als Subtext in `--text-faint`

---

## Color

Dark-only UI. Kein Light-Mode geplant (Power-User-Zielgruppe).

### Base Palette

```css
--bg-0: #0b0d10;   /* page background */
--bg-1: #14171b;   /* panels, cards */
--bg-2: #1b1f24;   /* inputs, elevated elements */
--bg-3: #23282f;   /* buttons, hover states */
--border: #2c323a; /* borders */
--text: #d5dae0;   /* primary text */
--text-dim: #8892a0;   /* secondary text */
--text-faint: #5b6472; /* metadata, hints */
```

### Accent

```css
--accent: #4da3ff;     /* primary accent — links, active states, progress */
--accent-dim: #2a5c8a; /* accent background, primary buttons */
```

### State Colors

```css
--state-queued:      #8892a0; /* neutral gray */
--state-downloading: #4da3ff; /* accent blue */
--state-paused:      #d8a94d; /* amber */
--state-done:        #4caf6f; /* green */
--state-error:       #e0555a; /* red */
```

---

## Typography

```css
--sans: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
--mono: 'SFMono-Regular', Consolas, 'Liberation Mono', Menlo, monospace;
```

- **UI-Text:** 13px sans, line-height 1.4
- **Metadaten / Progress:** 11–12px mono (Bytes, Speed, URLs, Timestamps)
- **Section-Header:** 11px uppercase, letter-spacing 0.05em, `--text-dim`
- **Kein Bold-Overuse** — font-weight 500 für Dateinamen, 400 für den Rest

---

## Spacing & Radius

```css
--radius: 3px; /* bewusst klein — kein rundes SaaS-Look */
```

- Padding panels: 12px
- Gap zwischen Elementen: 8px (klein), 14px (normal)
- Kein overdone Whitespace — IT-affine User wollen Dichte

---

## UI-Prinzipien

### Informationsdichte über Leerraum
Zielgruppe schätzt maximale Information auf Blick — keine großen Hero-Bereiche, keine leeren Seiten, kein Padding-Padding.

### Package-Gruppierung
Downloads sind immer in Packages gruppiert (auch Einzeldateien). Packages sind faltbar. Multi-Part RARs sind auf einen Blick als zusammengehörig erkennbar.

### Status-Farben konsequent
State-Colors werden *ausschliesslich* für die 5 Download-States verwendet — nirgendwo als Dekoration.

### Mono für alles Technische
URLs, Dateinamen, Grössen, Speeds, Timestamps → immer `--mono`. Visuelle Unterscheidung zwischen "Inhalt" und "Metadaten".

### Icons statt Text-Buttons
Action-Buttons (Pause, Cancel, Retry) als Icon-Buttons (26×26px). Text-Labels nur wo wirklich nötig.

---

## Stats-Bar (Startseite)

4 Metriken immer sichtbar oben:

| Metric | Farbe |
|---|---|
| Downloading | `--accent` |
| Queued | `--text-dim` |
| Done today | `--state-done` |
| Errors | `--state-error` |

---

## Komponenten-Stil

### Badges / State-Pills
```
border: 1px solid currentColor
border-radius: 10px
padding: 2px 8px
font-size: 11px
font-family: mono
```

### Progress Bar
- Einzelner Balken (4px, `--bg-3` Hintergrund, `--radius: 2px`), Füllstand = `downloaded / total`
- Farbe der Füllung wechselt je nach State (`--state-queued/downloading/paused/done/error`)
- Darunter: `downloaded / total · speed` in mono/faint

### Buttons
- Default: `--bg-3` Background, `--border` Border
- Primary: `--accent-dim` Background
- Danger-Hover: `--state-error` Border + Text
- Small: 3px 8px padding für Actions in Rows

---

## Was Hopper NICHT ist

- Kein buntes SaaS-Dashboard
- Kein Light-Mode (vorerst)
- Kein Onboarding-Wizard
- Keine Animationen ausser funktionale Fortschrittsbalken
- Kein Marketing-Speak in der UI

---

## Wording

| Kontext | Sprache |
|---|---|
| UI-Labels | Englisch |
| Fehlermeldungen | Englisch, technisch präzise |
| Docs / README | Englisch |
| Log-Output | Englisch, structured JSON |

Sentence case überall. Keine Title Case ausser Produktname.