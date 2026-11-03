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
const {
  accountBadge,
  accountNote,
  mailProgress,
  relativeTime,
  categoryLabel,
  connectNotice,
  categories,
} = compiled.exports;

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

const account = (over = {}) => ({
  status: 'active',
  last_error: '',
  retry_at: null,
  mails_done: 0,
  mails_total: 0,
  mails_relevant: 0,
  ...over,
});
const NOW = Date.parse('2026-10-28T12:00:00Z');

test('progress counts classified mail out of all mail found', () => {
  assert.deepEqual(plain(mailProgress(account({ mails_done: 3, mails_total: 12 }))), {
    done: 3,
    total: 12,
    percent: 25,
    working: true,
  });
  assert.equal(mailProgress(account({ mails_done: 12, mails_total: 12 })).working, false);
  assert.equal(mailProgress(account()).percent, 0);
  // A count that raced ahead of the total never shows more than 100%.
  assert.equal(mailProgress(account({ mails_done: 9, mails_total: 8 })).done, 8);
});

test('the status pill says what the mailbox is doing', () => {
  assert.equal(accountBadge(account()).label, 'Active');
  assert.equal(accountBadge(account({ mails_done: 1, mails_total: 5 })).label, 'Reading mail');
  assert.equal(accountBadge(account({ last_error: 'Sync failed.' })).tone, 'warn');
  assert.equal(
    accountBadge(account({ retry_at: '2026-10-28T12:05:00Z' })).label,
    'Waiting for Gmail',
  );
  assert.equal(accountBadge(account({ status: 'paused' })).label, 'Paused');
  assert.equal(accountBadge(account({ status: 'needs_reconnect' })).label, 'Needs reconnect');
});

test('account notes tell the member what to do', () => {
  assert.match(accountNote(account({ status: 'paused' }), NOW), /until you resume/);
  assert.match(accountNote(account({ status: 'needs_reconnect' }), NOW), /Reconnect/);
  assert.equal(accountNote(account({ last_error: 'Sync failed: x' }), NOW), 'Sync failed: x');
  assert.equal(accountNote(account(), NOW), '');
  assert.match(
    accountNote(account({ retry_at: '2026-10-28T12:02:00Z', last_error: 'ignored' }), NOW),
    /limiting requests.*in 2 min/,
  );
});

test('relative times read naturally in both directions', () => {
  assert.equal(relativeTime('2026-10-28T11:59:50Z', NOW), 'just now');
  assert.equal(relativeTime('2026-10-28T12:00:10Z', NOW), 'in a moment');
  assert.equal(relativeTime('2026-10-28T11:55:00Z', NOW), '5 min ago');
  assert.equal(relativeTime('2026-10-28T09:00:00Z', NOW), '3 h ago');
  assert.equal(relativeTime('2026-10-26T12:00:00Z', NOW), '2 d ago');
  assert.equal(relativeTime('2026-10-28T12:30:00Z', NOW), 'in 30 min');
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
