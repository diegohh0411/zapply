const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const assert = require('node:assert/strict');
const test = require('node:test');
const ts = require('typescript');
const source = fs.readFileSync(path.resolve(__dirname, '../src/app/mail/format.ts'), 'utf8');
const compiled = { exports: {} };
vm.runInNewContext(
  ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText,
  { module: compiled, exports: compiled.exports },
);
const plain = (value) => JSON.parse(JSON.stringify(value));
const { accountStatus, categoryLabel, connectNotice, categories } = compiled.exports;

test('every relevant category has a label and "other" is never offered', () => {
  assert.deepEqual(plain(categories.map((c) => c.value)), [
    'received',
    'assessment',
    'interview',
    'rejection',
    'offer',
  ]);
  assert.equal(categoryLabel('interview'), 'Interview');
});

test('account status tells the member what to do', () => {
  assert.match(accountStatus({ status: 'paused', last_error: '' }), /Paused/);
  assert.match(accountStatus({ status: 'needs_reconnect', last_error: '' }), /Reconnect/);
  assert.equal(accountStatus({ status: 'active', last_error: 'Sync failed: x' }), 'Sync failed: x');
  assert.equal(accountStatus({ status: 'active', last_error: '' }), 'Reading new mail');
});

test('the callback query becomes a banner', () => {
  assert.equal(connectNotice({}), null);
  assert.equal(connectNotice({ connected: '1' }).kind, 'ok');
  for (const error of ['denied', 'expired', 'in_use', 'unavailable', 'failed', 'whatever']) {
    const n = connectNotice({ error });
    assert.equal(n.kind, 'error');
    assert.ok(n.text.length > 10);
  }
  assert.match(connectNotice({ error: 'in_use' }).text, /another zapply account/);
});
