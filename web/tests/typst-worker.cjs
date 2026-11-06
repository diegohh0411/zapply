// Exercise the actual worker protocol, bundled for the browser, with local
// responses for the same public assets the API and Angular serve.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { pathToFileURL } = require('node:url');
const { Worker } = require('node:worker_threads');
const crypto = require('node:crypto');
const assert = require('node:assert/strict');
const test = require('node:test');
const { build } = require('esbuild');
const ts = require('typescript');
const vm = require('node:vm');
const root = path.resolve(__dirname, '../..');

async function start(t, { wasmFailures = 0 } = {}) {
  const outdir = fs.mkdtempSync(path.join(os.tmpdir(), 'zapply-worker-'));
  t.after(() => fs.rmSync(outdir, { recursive: true, force: true }));
  await build({
    entryPoints: [path.join(root, 'web/src/app/jobs/cv/typst.worker.ts')],
    bundle: true,
    format: 'esm',
    platform: 'browser',
    splitting: true,
    outdir,
    outExtension: { '.js': '.mjs' },
    logLevel: 'silent',
  });
  const worker = new Worker(
    `
    const { parentPort, workerData } = require('node:worker_threads');
    const fs = require('node:fs');
    const path = require('node:path');
    globalThis.self = globalThis;
    globalThis.fetch = async (url) => {
      parentPort.postMessage({ fetched: url });
      let file;
      if (url.startsWith('/assets/templates/')) file = path.join(workerData.root, 'templates', url.slice('/assets/templates/'.length));
      else if (url === '/typst/0.7.0/typst_ts_web_compiler_bg.wasm') {
        if (workerData.wasmFailures-- > 0) return new Response('', { status: 503 });
        file = workerData.wasm;
      }
      else throw new Error('Unexpected external fetch: ' + url);
      return fs.existsSync(file)
        ? new Response(fs.readFileSync(file), { headers: { 'Content-Type': file.endsWith('.wasm') ? 'application/wasm' : 'application/octet-stream' } })
        : new Response('', { status: 404 });
    };
    globalThis.addEventListener = (event, listener) => parentPort.on(event, data => listener({ data }));
    globalThis.postMessage = (data, transfer) => parentPort.postMessage(data, transfer);
    import(workerData.module).then(() => parentPort.postMessage({ ready: true }));
  `,
    {
      eval: true,
      workerData: {
        root,
        wasmFailures,
        module: pathToFileURL(path.join(outdir, 'typst.worker.mjs')).href,
        wasm: require.resolve('@myriaddreamin/typst-ts-web-compiler/pkg/typst_ts_web_compiler_bg.wasm'),
      },
    },
  );
  t.after(() => worker.terminate());
  await new Promise((resolve, reject) => {
    worker.once('message', resolve);
    worker.once('error', reject);
  });
  let seq = 0;
  const pending = new Map();
  const fetches = [];
  worker.on('message', (data) => {
    if (data.fetched) {
      fetches.push(data.fetched);
      return;
    }
    const p = pending.get(data.id);
    pending.delete(data.id);
    if (data.error) p.reject(new Error(data.error));
    else p.resolve(data);
  });
  worker.on('error', (error) => {
    for (const p of pending.values()) p.reject(error);
  });
  const manifest = JSON.parse(
    fs.readFileSync(path.join(root, 'templates/classic/1/manifest.json')),
  );
  const fontDir = 'fonts/' + manifest.fonts;
  const template = {
    main: '/assets/templates/classic/1/main.typ',
    strings: '/assets/templates/classic/1/strings.json',
    fonts: fs
      .readdirSync(path.join(root, 'templates', fontDir))
      .filter((f) => /\.(ttf|otf)$/.test(f))
      .map((f) => '/assets/templates/' + fontDir + '/' + f),
  };
  const compile = (content, assets = template) =>
    new Promise((resolve, reject) => {
      const id = ++seq;
      pending.set(id, { resolve, reject });
      worker.postMessage({ id, template: assets, content });
    });
  compile.preload = () => worker.postMessage({ type: 'preload' });
  compile.fetches = fetches;
  return compile;
}
const hash = (bytes) => crypto.createHash('sha256').update(bytes).digest('hex');

test('preloading shares one streamed WASM module with queued compiles', async (t) => {
  const compile = await start(t);
  compile.preload();
  compile.preload();
  const content = JSON.parse(fs.readFileSync(path.join(root, 'templates/fixtures/full-en.json')));
  const results = await Promise.all([compile(content), compile(content)]);
  const golden = JSON.parse(fs.readFileSync(path.join(root, 'templates/golden.json')));
  for (const result of results) assert.equal(hash(result.pdf), golden['classic/1/full-en']);
  assert.equal(compile.fetches.filter((url) => url.endsWith('.wasm')).length, 1);
});

test('a failed WASM fetch can be retried', async (t) => {
  const compile = await start(t, { wasmFailures: 1 });
  const content = JSON.parse(fs.readFileSync(path.join(root, 'templates/fixtures/full-en.json')));
  await assert.rejects(compile(content), /Couldn't load.*503/);
  assert.ok((await compile(content)).pdf.length > 0);
  assert.equal(compile.fetches.filter((url) => url.endsWith('.wasm')).length, 2);
});

test(
  'browser worker reports pages and reproduces native golden PDFs across queued requests',
  { timeout: 60000 },
  async (t) => {
    const compile = await start(t);
    const golden = JSON.parse(fs.readFileSync(path.join(root, 'templates/golden.json')));
    const fixtures = Object.keys(golden).filter((k) => k.startsWith('classic/1/'));
    const requests = fixtures.flatMap((key) => {
      const content = JSON.parse(
        fs.readFileSync(path.join(root, 'templates/fixtures', key.split('/').at(-1) + '.json')),
      );
      return [1, 2].map(async () => {
        const result = await compile(content);
        assert.equal(hash(result.pdf), golden[key]);
        assert.ok(Number.isInteger(result.pages) && result.pages > 0);
      });
    });
    await Promise.all(requests);
    await assert.rejects(
      compile(
        {},
        {
          main: '/assets/templates/missing.typ',
          strings: '/assets/templates/missing.json',
          fonts: [],
        },
      ),
      /Couldn't load/,
    );
    const content = JSON.parse(fs.readFileSync(path.join(root, 'templates/fixtures/full-en.json')));
    const before = await compile(content);
    const module = { exports: {} };
    vm.runInNewContext(
      ts.transpileModule(
        fs.readFileSync(path.join(root, 'web/src/app/jobs/cv/cv-content.ts'), 'utf8'),
        {
          compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
        },
      ).outputText,
      { module, exports: module.exports },
    );
    const { fitContent, applyDrops } = module.exports;
    const trim = content.sections.flatMap((s) => s.entries.map((e) => e.id));
    const result = await fitContent(content, trim, 1,
      async dropped => (await compile(applyDrops(content, dropped))).pages);
    assert.ok(before.pages > 1, 'fixture must need fitting');
    assert.ok(result.pages <= 1);
    assert.equal((await compile(applyDrops(content, result.dropped))).pages, result.pages);
    // Every remaining omitted top-level entry was tried: none can be added
    // back to the final PDF without overflowing the selected page budget.
    for (const id of result.dropped) {
      const restored = result.dropped.filter(x => x !== id);
      assert.ok((await compile(applyDrops(content, restored))).pages > 1);
    }
  },
);
