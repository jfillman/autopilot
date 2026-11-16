# Autopilot brand

The mark is the Hangar **Instrument Dial** (ring, eight ticks, heading bug at 12 o'clock) with the Autopilot glyph
in the centre: a nose held between brackets, the way a heading-hold mode reads on the flight deck. **Amber**, the
Hangar colour for the products that need attention.

| File | Use |
|---|---|
| `autopilot-tile.svg` | Fixed instrument-black rounded square. README headers, catalog tiles, avatars. The default. |
| `autopilot-app-logo.svg`, `autopilot-app-logo-512.png` | The GitHub App's logo (`hangar-autopilot-agent`: Settings, Display information; GitHub wants a square image under 1 MB). The tile's mark on a full-bleed `#0B0D10` ground, sized to sit inside GitHub's circular crop. Set the App's badge background color to `#0B0D10`. Re-render after editing the SVG (macOS): `qlmanage -t -s 512 -o /tmp docs/brand/autopilot-app-logo.svg && cp /tmp/autopilot-app-logo.svg.png docs/brand/autopilot-app-logo-512.png` |
| `autopilot-social-preview.png` | 2560 x 1280 social preview. **Upload by hand** in Settings, General, Social preview: GitHub has no API for it. |
| `autopilot-logo-options.html` | The five options that were considered (Hold, Flight director, Annunciator, Corridor, Loop) and why Hold was chosen. Open it in a browser. |

The transparent `autopilot-inline.svg` lives with the other products' marks in Hangar's
[`brand/marks/`](https://github.com/jfillman/hangar/tree/main/brand/marks) (and Tower's). Palette, type and the family
rules are in the Hangar Brand System, [`docs/brand/`](https://github.com/jfillman/hangar/tree/main/docs/brand).
Clearance, Preflight and Flight recorder are features of Autopilot and have no mark of their own.
