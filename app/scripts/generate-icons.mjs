// Generates KARMAX brand assets (icon, adaptive, splash, favicon) from an SVG
// "terminal prompt" mark: an amber chevron + block cursor on ink.
// Run: node scripts/generate-icons.mjs
import { Resvg } from '@resvg/resvg-js';
import { writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const OUT = join(ROOT, 'assets', 'images');

const INK = '#0a0d12';
const AMBER = '#f2b43a';

function svg({ bg = false, mark = AMBER, drawMark = true }) {
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024">
  ${bg ? `<rect width="1024" height="1024" fill="${INK}"/>` : ''}
  ${
    drawMark
      ? `<polyline points="320,330 540,512 320,694" fill="none" stroke="${mark}" stroke-width="74" stroke-linecap="round" stroke-linejoin="round"/>
  <rect x="592" y="436" width="112" height="150" rx="10" fill="${mark}"/>`
      : ''
  }
</svg>`;
}

function render(file, markup, size) {
  const r = new Resvg(markup, { fitTo: { mode: 'width', value: size }, background: 'rgba(0,0,0,0)' });
  writeFileSync(join(OUT, file), r.render().asPng());
  console.log('wrote', file, size);
}

render('icon.png', svg({ bg: true, mark: AMBER }), 1024);
render('favicon.png', svg({ bg: true, mark: AMBER }), 48);
render('android-icon-foreground.png', svg({ bg: false, mark: AMBER }), 1024);
render('android-icon-background.png', svg({ bg: true, drawMark: false }), 1024);
render('android-icon-monochrome.png', svg({ bg: false, mark: '#ffffff' }), 1024);
render('splash-icon.png', svg({ bg: false, mark: AMBER }), 512);
console.log('done');
