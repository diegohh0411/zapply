/// <reference lib="webworker" />
// Compiles CVs with Typst (WASM) off the main thread. One compile returns the
// PDF and its page count, which fitting needs (docs/parts/07-rendering.md).
import { createTypstCompiler, type TypstCompiler } from '@myriaddreamin/typst.ts/compiler';
import { disableDefaultFontAssets, loadFonts } from '@myriaddreamin/typst.ts/options.init';

import type { CompileRequest, CompileResponse } from './typst-protocol';

// Copied from node_modules by angular.json; the directory is the typst.ts
// release, which pins the Typst version (web/tests/typst-golden.cjs).
const WASM = '/typst/0.7.0/typst_ts_web_compiler_bg.wasm';

const compilers = new Map<string, Promise<TypstCompiler>>();
const sources = new Map<string, Promise<Uint8Array>>();

function fetchBytes(url: string): Promise<Uint8Array> {
  let p = sources.get(url);
  if (!p) {
    p = fetch(url).then(async (r) => {
      if (!r.ok) throw new Error(`Couldn't load ${url} (${r.status})`);
      return new Uint8Array(await r.arrayBuffer());
    });
    p.catch(() => sources.delete(url));
    sources.set(url, p);
  }
  return p;
}

// Fonts load when a compiler is built, so there is one compiler per font set.
function compiler(fonts: string[]): Promise<TypstCompiler> {
  const key = fonts.join('\n');
  let p = compilers.get(key);
  if (!p) {
    p = (async () => {
      const data = await Promise.all(fonts.map(fetchBytes));
      const c = createTypstCompiler();
      await c.init({
        getModule: () => ({ module_or_path: WASM }),
        beforeBuild: [disableDefaultFontAssets(), loadFonts(data)],
      });
      return c;
    })();
    p.catch(() => compilers.delete(key));
    compilers.set(key, p);
  }
  return p;
}

const encoder = new TextEncoder();

async function compile(req: CompileRequest): Promise<CompileResponse> {
  const [c, main, strings] = await Promise.all([
    compiler(req.template.fonts),
    fetchBytes(req.template.main),
    fetchBytes(req.template.strings),
  ]);
  c.resetShadow();
  c.addSource('/main.typ', new TextDecoder().decode(main));
  c.mapShadow('/strings.json', strings);
  c.mapShadow('/content.json', encoder.encode(JSON.stringify(req.content)));
  return c.runWithWorld({ mainFilePath: '/main.typ', inputs: {} }, async (world) => {
    const result = await world.compile({ diagnostics: 'unix' });
    if (result.hasError)
      throw new Error(`The CV template failed: ${JSON.stringify(result.diagnostics)}`);
    const pages = (await world.query({ selector: '<cv-pages>', field: 'value' })) as number[];
    const pdf = (await world.pdf()).result;
    if (!pdf) throw new Error('The CV compiled but produced no PDF');
    return { id: req.id, pages: pages[0] ?? 1, pdf };
  });
}

// Compiles share one compiler's shadow files, so they run one at a time.
let queue: Promise<unknown> = Promise.resolve();
addEventListener('message', ({ data }: MessageEvent<CompileRequest>) => {
  queue = queue.then(async () => {
    try {
      const out = await compile(data);
      postMessage(out, [out.pdf!.buffer]);
    } catch (e) {
      postMessage({ id: data.id, error: (e as Error).message } satisfies CompileResponse);
    }
  });
});
