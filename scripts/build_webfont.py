"""Build the bundled fallback from NotoSansMonoCJKsc-Regular.otf.

Requires fonttools[woff]. Only characters used by the interface are bundled.
Other characters use the device fallback; no third-party requests are made.
"""
from pathlib import Path
import hashlib
import sys
from fontTools import subset
from fontTools.ttLib import TTFont

ROOT = Path(__file__).resolve().parents[1]
SOURCE = Path(sys.argv[1])
OUT = ROOT / 'docs' / 'assets' / 'fonts'
OUT.mkdir(parents=True, exist_ok=True)


def ranges(points):
    groups = []
    for point in sorted(points):
        if groups and point == groups[-1][1] + 1:
            groups[-1][1] = point
        else:
            groups.append([point, point])
    return ','.join(f'U+{a:X}' if a == b else f'U+{a:X}-{b:X}' for a, b in groups)


with TTFont(SOURCE) as original:
    supported = set(original.getBestCmap())
    copyright_text = original['name'].getDebugName(0)

characters = set(range(32, 127))
for path in [ROOT / 'index.html', ROOT / 'docs/index.html', ROOT / 'status-page.js', *ROOT.glob('*.go')]:
    characters.update(map(ord, path.read_text(encoding='utf-8-sig')))
common = characters & supported
rules = []
for name, points in [('ui', common)]:
    with TTFont(SOURCE) as font:
        options = subset.Options()
        options.name_IDs = ['*']
        options.name_languages = ['*']
        worker = subset.Subsetter(options=options)
        worker.populate(unicodes=points)
        worker.subset(font)
        for record in font['name'].names:
            value = {1: 'Nmb Gothic', 2: 'Regular', 3: f'Nmb Gothic v2 {name}',
                     4: 'Nmb Gothic Regular', 6: 'NmbGothic-Regular',
                     16: 'Nmb Gothic', 17: 'Regular'}.get(record.nameID)
            if value:
                record.string = value.encode(record.getEncoding())
        font.flavor = 'woff2'
        filename = f'nmb-gothic-{name}-v2.woff2'
        font.save(OUT / filename)
    rules.append('@font-face{font-family:"Nmb Gothic";font-style:normal;font-weight:400;'
                 f'font-display:swap;src:url("{filename}") format("woff2");'
                 f'unicode-range:{ranges(points)}' + '}')
    print(f'{filename}: {len(points)} characters, {(OUT / filename).stat().st_size} bytes')
(OUT / 'fonts.css').write_text('\n'.join(rules) + '\n', encoding='utf-8')
(OUT / 'README.txt').write_text(
    'Nmb Gothic: subsetted web fallback derived from Noto Sans Mono CJK SC Regular.\n'
    'This is not Microsoft MS Gothic.\n'
    'Source: https://github.com/notofonts/noto-cjk/tree/main/Sans/Mono\n'
    f'Source SHA256: {hashlib.sha256(SOURCE.read_bytes()).hexdigest()}\n'
    f'{copyright_text}\n'
    'License: SIL Open Font License 1.1, included in LICENSE.txt.\n'
    'This subset covers the interface and built-in probe messages. Other\n'
    'characters fall back to the device font.\n', encoding='utf-8')
