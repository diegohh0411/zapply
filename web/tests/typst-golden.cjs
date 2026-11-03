// The WASM half of the determinism gate (docs/parts/07-rendering.md).
// templates/golden.json holds the SHA-256 of each fixture compiled by the
// native Typst CLI (templates/gate_test.go). The browser's compiler must
// produce the same bytes.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const test = require('node:test');

const templates = path.resolve(__dirname, '../../templates');
// The Typst version each typst.ts release compiles with. A template can only
// run in the browser if its pinned version is the one shipped here.
const typstOf = { '0.7.0': '0.14.2' };

const read = (...p) => fs.readFileSync(path.join(templates, ...p));
const golden = JSON.parse(read('golden.json'));
const manifests = fs
  .readdirSync(templates, { withFileTypes: true })
  .filter((d) => d.isDirectory() && !['fonts', 'fixtures'].includes(d.name))
  .flatMap((d) =>
    fs.readdirSync(path.join(templates, d.name)).map((v) => JSON.parse(read(d.name, v, 'manifest.json'))),
  );
const fixtures = fs
  .readdirSync(path.join(templates, 'fixtures'))
  .filter((f) => f.endsWith('.json'))
  .map((f) => ({ name: f.slice(0, -5), bytes: read('fixtures', f) }));

const compilerVersion = require('@myriaddreamin/typst-ts-web-compiler/package.json').version;

async function compilerFor(manifest) {
  const { createTypstCompiler } = await import('@myriaddreamin/typst.ts');
  const { disableDefaultFontAssets, loadFonts } = await import(
    '@myriaddreamin/typst.ts/dist/esm/options.init.mjs'
  );
  const wasm = fs.readFileSync(
    require.resolve('@myriaddreamin/typst-ts-web-compiler/pkg/typst_ts_web_compiler_bg.wasm'),
  );
  const fontDir = path.join(templates, 'fonts', manifest.fonts);
  const fonts = fs
    .readdirSync(fontDir)
    .filter((f) => f.endsWith('.ttf') || f.endsWith('.otf'))
    .map((f) => new Uint8Array(fs.readFileSync(path.join(fontDir, f))));
  const compiler = createTypstCompiler();
  await compiler.init({
    getModule: () => ({ module_or_path: wasm }),
    beforeBuild: [disableDefaultFontAssets(), loadFonts(fonts)],
  });
  return compiler;
}

for (const manifest of manifests) {
  const dir = `${manifest.name}/${manifest.version}`;
  test(`${dir} pins the Typst version typst.ts ships`, () => {
    assert.equal(manifest.typst, typstOf[compilerVersion], `typst.ts ${compilerVersion}`);
  });

  test(`${dir} compiles every fixture to the golden hash`, async () => {
    const compiler = await compilerFor(manifest);
    for (const f of fixtures) {
      const key = `${dir}/${f.name}`;
      if (!(key in golden)) continue;
      compiler.resetShadow();
      compiler.addSource('/main.typ', read(dir, 'main.typ').toString('utf8'));
      compiler.mapShadow('/strings.json', new Uint8Array(read(dir, 'strings.json')));
      compiler.mapShadow('/content.json', new Uint8Array(f.bytes));
      const out = await compiler.compile({ mainFilePath: '/main.typ', format: 1, diagnostics: 'unix' });
      assert.ok(out.result, `${key}: ${JSON.stringify(out.diagnostics)}`);
      const hash = crypto.createHash('sha256').update(out.result).digest('hex');
      assert.equal(hash, golden[key], key);
    }
  });
}
