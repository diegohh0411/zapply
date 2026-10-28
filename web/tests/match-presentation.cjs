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
const { relevanceLabel } = compiled.exports;

test('experience relevance uses the selection threshold and whole-posting scale', () => {
  assert.equal(relevanceLabel(1), 'Strongly relevant');
  assert.equal(relevanceLabel(0.75), 'Strongly relevant');
  assert.equal(relevanceLabel(0.5), 'Relevant');
  assert.equal(relevanceLabel(0.375), 'Relevant');
  assert.equal(relevanceLabel(0.374), 'Less relevant');
  assert.equal(relevanceLabel(0), 'Less relevant');
});
