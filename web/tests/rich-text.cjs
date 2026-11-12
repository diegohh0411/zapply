const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const assert = require('node:assert/strict');
const test = require('node:test');
const ts = require('typescript');
const source = fs.readFileSync(path.resolve(__dirname, '../src/app/shared/rich-text/rich-text.ts'), 'utf8');
const compiled = { exports: {} };
vm.runInNewContext(
  ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText,
  { module: compiled, exports: compiled.exports },
);
const { parseRich, serializeRich, plainRich } = compiled.exports;
const plain = (v) => JSON.parse(JSON.stringify(v));
const run = (text, bold = false, italic = false) => ({ text, bold, italic });

test('parses strong, emphasis and escapes like the CV template', () => {
  assert.deepEqual(plain(parseRich('Cut *40%* with _fast_ builds')), [
    run('Cut '), run('40%', true), run(' with '), run('fast', false, true), run(' builds'),
  ]);
  assert.deepEqual(plain(parseRich('*_both_*')), [run('both', true, true)]);
  assert.deepEqual(plain(parseRich('a \\* star and snake\\_case')), [run('a * star and snake_case')]);
});

test('unbalanced marks keep the text as given', () => {
  assert.deepEqual(plain(parseRich('5* rating')), [run('5* rating')]);
  assert.deepEqual(plain(parseRich('')), []);
});

test('serializing escapes literal marks and round-trips', () => {
  const runs = [run('Built '), run(' 96 articles ', true), run('and '), run('snake_case', false, true), run(' 5*')];
  const s = serializeRich(runs);
  assert.equal(s, 'Built  *96 articles* and _snake\\_case_ 5\\*');
  assert.equal(plainRich(s), 'Built  96 articles and snake_case 5*');
  assert.equal(serializeRich(plain(parseRich(s))), s);
  assert.equal(serializeRich([run('x', true), run('y', true, true)]), '*x**_y_*');
  assert.deepEqual(plain(parseRich('*x**_y_*')), [run('x', true), run('y', true, true)]);
});
