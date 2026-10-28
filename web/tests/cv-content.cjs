const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const assert = require('node:assert/strict');
const test = require('node:test');
const ts = require('typescript');
const source = fs.readFileSync(path.resolve(__dirname, '../src/app/jobs/cv/cv-content.ts'), 'utf8');
const compiled = { exports: {} };
vm.runInNewContext(
  ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText,
  { module: compiled, exports: compiled.exports },
);
const { fit, applyDrops, filename } = compiled.exports;
const plain = (value) => JSON.parse(JSON.stringify(value));

test('fitting finds the first fitting prefix and uses logarithmically many compiles', async () => {
  for (const n of [0, 1, 2, 10, 100]) {
    for (let threshold = 0; threshold <= n + 1; threshold++) {
      const calls = [];
      const result = await fit(n, 2, async (k) => {
        calls.push(k);
        assert.ok(k >= 0 && k <= n);
        return k < threshold ? 3 : 2;
      });
      assert.equal(result.k, Math.min(threshold, n));
      assert.equal(result.pages, threshold > n ? 3 : 2);
      assert.equal(new Set(calls).size, calls.length);
      assert.ok(calls.length <= Math.ceil(Math.log2(Math.max(n, 1))) + 2);
    }
  }
});

test('dropping removes nested content, empty sections and skills without changing its input', () => {
  const content = {
    schema: 1,
    locale: 'en',
    date: '2026-10-28',
    profile: { name: 'Ada' },
    sections: [
      {
        key: 'experience',
        entries: [
          { id: 'parent', highlights: [{ id: 'h1' }, { id: 'h2' }], children: [{ id: 'child' }] },
        ],
      },
      { key: 'education', entries: [{ id: 'degree' }] },
    ],
    skills: [{ id: 'go' }, { id: 'sql' }],
  };
  const original = JSON.stringify(content);
  const out = applyDrops(content, ['h2', 'child', 'degree', 'sql']);
  assert.deepEqual(plain(out.sections), [
    { key: 'experience', entries: [{ id: 'parent', highlights: [{ id: 'h1' }] }] },
  ]);
  assert.deepEqual(plain(out.skills), [{ id: 'go' }]);
  assert.equal(JSON.stringify(content), original);
  assert.equal(applyDrops(content, ['parent']).sections[0].key, 'education');
});

test('download filenames keep names while removing file-system separators', () => {
  assert.equal(filename(' Ada ', 'Acme/Labs'), 'Ada - AcmeLabs.pdf');
  assert.equal(filename('', ''), 'CV.pdf');
});
