const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const assert = require('node:assert/strict');
const test = require('node:test');
const ts = require('typescript');
const source = fs.readFileSync(
  path.resolve(__dirname, '../src/app/jobs/match/match-presentation.ts'),
  'utf8',
);
const code = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText;
const compiled = { exports: {} };
vm.runInNewContext(code, { module: compiled, exports: compiled.exports });
const { coverageGroup, coverageLabel, primaryRequirements } = compiled.exports;
const req = (text_hash, overrides = {}) => ({
  text_hash,
  class: 'must',
  status: 'met',
  provisional: false,
  review: false,
  ...overrides,
});

test('uncertain and unfinished matches are never presented as supported or definitive gaps', () => {
  for (const item of [
    req('a', { review: true }),
    req('b', { provisional: true }),
    req('c', { status: 'unknown' }),
    req('d', { status: 'gap', provisional: true }),
  ]) {
    assert.equal(coverageGroup(item), 'review');
    assert.equal(coverageLabel(item), 'Needs review');
  }
  assert.equal(coverageGroup(req('e', { status: 'partial' })), 'review');
  assert.equal(coverageLabel(req('f', { status: 'adjacent' })), 'Some evidence');
  assert.equal(coverageLabel(req('g', { status: 'gap' })), 'No evidence found');
});

test('posting context is separated and must-have gaps appear before strengths without mutating results', () => {
  const items = [
    req('context', { class: 'trait' }),
    req('supported'),
    req('nice-gap', { class: 'preferred', status: 'gap' }),
    req('must-gap', { status: 'gap' }),
    req('ignored', { class: 'ignore', status: 'ignored' }),
    req('duty', { class: 'duty' }),
  ];
  assert.deepEqual(
    Array.from(primaryRequirements(items), (item) => item.text_hash),
    ['must-gap', 'supported', 'nice-gap', 'duty'],
  );
  assert.equal(items[0].text_hash, 'context');
  assert.equal(coverageGroup(items[4]), 'ignored');
  assert.equal(
    primaryRequirements(items.filter((item) => ['trait', 'ignore'].includes(item.class))).length,
    0,
  );
});
