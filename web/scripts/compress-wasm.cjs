// Compress once at build time, rather than on the first CV request.
const fs = require('node:fs/promises');
const path = require('node:path');
const { promisify } = require('node:util');
const zlib = require('node:zlib');

async function main() {
  const root = path.resolve(__dirname, '../dist/web/browser/typst');
  const files = (await fs.readdir(root, { recursive: true })).filter((f) => f.endsWith('.wasm'));
  if (!files.length) throw new Error('No Typst WASM assets in the web build');
  for (const file of files) {
    const target = path.join(root, file);
    const bytes = await fs.readFile(target);
    const [br, gz] = await Promise.all([
      promisify(zlib.brotliCompress)(bytes, {
        params: { [zlib.constants.BROTLI_PARAM_QUALITY]: 9 },
      }),
      promisify(zlib.gzip)(bytes, { level: 9 }),
    ]);
    await Promise.all([fs.writeFile(target + '.br', br), fs.writeFile(target + '.gz', gz)]);
    console.log(`${file}: ${bytes.length} bytes → ${br.length} Brotli / ${gz.length} gzip`);
  }
}

main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
