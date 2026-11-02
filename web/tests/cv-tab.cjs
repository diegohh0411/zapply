const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const test = require('node:test');
const ts = require('typescript');
const { Subject } = require('rxjs');
const signal = (value) =>
  Object.assign(() => value, {
    set: (next) => {
      value = next;
    },
  });
const deferred = () => {
  let resolve;
  const promise = new Promise((r) => {
    resolve = r;
  });
  return { promise, resolve };
};
class AuthService {}
class EventsService {}
class TypstService {}
class DomSanitizer {}
class DestroyRef {}
class ApiError extends Error {
  constructor(status) {
    super('request failed');
    this.status = status;
  }
}
class Api {
  static ok(r) {
    if (!r.response.ok) throw new ApiError(r.response.status);
    return r.data;
  }
}
const ok = (data) => ({ data, response: { ok: true } });
const content = {
  schema: 1,
  date: '2026-10-28',
  locale: 'en',
  profile: { name: 'Ada' },
  sections: [],
  skills: [],
};
const preview = (version) => ({
  content,
  content_hash: 'hash',
  trim: [],
  template: { assets: {} },
  draft: {
    version,
    page_budget: 1,
    template: 'classic',
    locale: 'en',
    picks: { include: [], exclude: [] },
  },
});
function transpile(file, requireStub) {
  let source = fs.readFileSync(path.resolve(__dirname, '../src/app/jobs/cv', file), 'utf8');
  const start = source.indexOf('@Component(');
  if (start >= 0)
    source = source.slice(0, start) + source.slice(source.indexOf('export class', start));
  const module = { exports: {} };
  vm.runInNewContext(
    ts.transpileModule(source, {
      compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
    }).outputText,
    {
      module,
      exports: module.exports,
      require: requireStub,
      URL,
      Blob,
      crypto: require('node:crypto').webcrypto,
      setTimeout,
      clearTimeout,
    },
  );
  return module.exports;
}
function fixture(
  client,
  compile = async () => ({ pages: 1, pdf: new Uint8Array([37, 80, 68, 70]) }),
) {
  const auth = { identity: signal('alice') };
  const effects = [];
  let destroy;
  const input = (value) => signal(value);
  input.required = () => signal('job');
  const core = {
    signal,
    computed: (fn) => fn,
    input,
    effect: (fn) => effects.push(fn),
    untracked: (fn) => fn(),
    DestroyRef,
    inject: (token) => {
      if (token === AuthService) return auth;
      if (token === EventsService) return { changes: new Subject(), resync: new Subject() };
      if (token === TypstService) return { compile, preload() {} };
      if (token === DomSanitizer) return { bypassSecurityTrustResourceUrl: (url) => url };
      if (token === DestroyRef)
        return {
          onDestroy: (fn) => {
            destroy = fn;
          },
        };
      return { client };
    },
  };
  const helper = transpile('cv-content.ts', () => ({}));
  const { CvTab } = transpile('cv-tab.ts', (name) => {
    if (name === '@angular/core') return core;
    if (name === '@angular/core/rxjs-interop') return { takeUntilDestroyed: () => (x) => x };
    if (name === '@angular/platform-browser') return { DomSanitizer };
    if (name.endsWith('/api')) return { Api, ApiError };
    if (name.endsWith('/auth.service')) return { AuthService };
    if (name.endsWith('/events.service')) return { EventsService };
    if (name === './typst.service') return { TypstService };
    if (name === './cv-content') return helper;
    return {};
  });
  const tab = new CvTab();
  return { tab, auth, effects, destroy: () => destroy() };
}

test('account switch rejects late CV previews and history before the identity effect runs', async () => {
  const pending = deferred();
  const f = fixture({ GET: () => pending.promise });
  const loading = f.tab.load('job');
  const history = f.tab.loadExports('job');
  f.auth.identity.set('bob');
  pending.resolve(ok(preview(1)));
  await Promise.all([loading, history]);
  assert.equal(f.tab.preview(), undefined);
  assert.equal(f.tab.exports().length, 0);
  f.destroy();
});

test('newest CV preview wins and a failed refit cannot export the previous PDF', async () => {
  const pending = deferred();
  let requests = 0;
  let fail = false;
  const f = fixture(
    { GET: () => (++requests === 1 ? pending.promise : Promise.resolve(ok(preview(requests)))) },
    async () => {
      if (fail) throw Error('compile failed');
      return { pages: 1, pdf: new Uint8Array([37, 80, 68, 70]) };
    },
  );
  const older = f.tab.load('job');
  await f.tab.load('job');
  pending.resolve(ok(preview(1)));
  await older;
  assert.equal(f.tab.preview().draft.version, 2);
  assert.ok(f.tab.fitted());
  fail = true;
  await f.tab.load('job');
  assert.equal(f.tab.fitted(), undefined);
  assert.equal(f.tab.error(), 'compile failed');
  assert.equal(f.tab.fitting(), false);
  f.destroy();
});

test('412 CV draft recovery reloads current content and releases Save', async () => {
  const f = fixture({
    GET: async () => ok(preview(2)),
    PUT: async () => ({ response: { ok: false, status: 412 } }),
  });
  f.tab.preview.set(preview(1));
  await f.tab.save({ page_budget: 2 });
  assert.equal(f.tab.preview().draft.version, 2);
  assert.equal(f.tab.busy(), false);
  assert.equal(f.tab.fitting(), false);
  assert.ok(f.tab.fitted());
  assert.match(f.tab.notice(), /another tab/);
  f.destroy();
});

test('destroying a CV tab prevents a pending compile from restoring its PDF', async () => {
  const pending = deferred();
  const f = fixture({}, () => pending.promise);
  f.tab.preview.set(preview(1));
  const fitting = f.tab.refit();
  f.destroy();
  pending.resolve({ pages: 1, pdf: new Uint8Array([37, 80, 68, 70]) });
  await fitting;
  assert.equal(f.tab.fitted(), undefined);
});

test('a delayed CV save cannot replace a newer preview received during the save', async () => {
  const pending = deferred();
  const f = fixture({ GET: async () => ok(preview(3)), PUT: () => pending.promise });
  f.tab.preview.set(preview(1));
  const versions = [];
  const set = f.tab.preview.set;
  f.tab.preview.set = next => { versions.push(next.draft.version); set(next); };
  const saving = f.tab.save({ page_budget: 2 });
  // Another tab's save has committed and its change event reloads version 3
  // before this tab receives the delayed version-2 PUT response.
  await f.tab.load('job');
  assert.equal(f.tab.preview().draft.version, 3);
  pending.resolve(ok(preview(2)));
  await saving;
  assert.equal(f.tab.preview().draft.version, 3);
  assert.ok(!versions.includes(2), 'the old response must never reach the view');
  assert.equal(f.tab.busy(), false);
  assert.ok(f.tab.fitted());
  f.destroy();
});

test('summary generation shows progress, preserves text and prevents duplicate runs', async () => {
  let saves = 0;
  const p = { ...preview(1), summary_status: 'running' };
  const f = fixture({ GET: async () => ok(p), PUT: async () => { saves++; return ok(p); } });
  f.tab.summaryText = 'Unsaved personal summary';
  await f.tab.load('job');
  assert.equal(f.tab.summaryGenerating(), true);
  assert.match(f.tab.summaryMessage(), /Generating summary/);
  assert.equal(f.tab.summaryText, 'Unsaved personal summary');
  f.tab.regenerateSummary();
  assert.equal(saves, 0);
  f.tab.preview.set({ ...p, summary_status: 'completed' });
  assert.equal(f.tab.summaryGenerating(), false);
  assert.match(f.tab.summaryMessage(), /updated/);
  f.destroy();
});

test('failed and blocked summary runs show actionable messages without a spinner', () => {
  const f = fixture({});
  f.tab.preview.set({ ...preview(1), summary_status: 'failed', summary_reason: 'result_unavailable' });
  assert.equal(f.tab.summaryGenerating(), false);
  assert.match(f.tab.summaryMessage(), /Try Regenerate/);
  f.tab.preview.set({ ...preview(1), summary_status: 'blocked', summary_reason: 'usage_limit' });
  assert.equal(f.tab.summaryGenerating(), false);
  assert.match(f.tab.summaryMessage(), /allowance/);
  f.destroy();
});
