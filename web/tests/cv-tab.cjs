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

test('newest CV preview wins and a failed refit preserves the PDF but prevents export', async () => {
  const pending = deferred();
  let requests = 0;
  let fail = false;
  const f = fixture(
    {
      GET: () =>
        ++requests === 1
          ? pending.promise
          : Promise.resolve(
              ok({
                ...preview(requests),
                content: { ...content, profile: { name: `Ada ${requests}` } },
              }),
            ),
      POST: () => assert.fail('a stale PDF cannot be exported'),
    },
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
  const previous = f.tab.fitted();
  fail = true;
  await f.tab.load('job');
  assert.equal(f.tab.fitted(), previous);
  assert.equal(f.tab.pdfCurrent(), false);
  assert.equal(f.tab.error(), 'compile failed');
  assert.equal(f.tab.fitting(), false);
  await f.tab.export();
  f.destroy();
});

test('summary polls keep the existing PDF and only compile changed inputs in the background', async () => {
  let p = { ...preview(1), summary_status: 'running' };
  let compilations = 0;
  const pending = deferred();
  const f = fixture({ GET: async () => ok(p) }, () => {
    compilations++;
    return compilations === 1
      ? Promise.resolve({ pages: 1, pdf: new Uint8Array([1]) })
      : pending.promise;
  });
  await f.tab.load('job');
  const previous = f.tab.fitted();
  await f.tab.load('job');
  assert.equal(compilations, 1);
  assert.equal(f.tab.fitted(), previous);
  assert.equal(f.tab.pdfCurrent(), true);

  p = {
    ...p,
    summary_status: 'completed',
    content: {
      ...content,
      profile: { ...content.profile, summary: 'Updated summary' },
    },
  };
  const loading = f.tab.load('job');
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(compilations, 2);
  assert.equal(f.tab.fitted(), previous);
  assert.equal(f.tab.fitting(), true);
  assert.equal(f.tab.pdfCurrent(), false);
  pending.resolve({ pages: 1, pdf: new Uint8Array([2]) });
  await loading;
  assert.notEqual(f.tab.fitted().url, previous.url);
  assert.equal(f.tab.fitted().pdf[0], 2);
  assert.equal(f.tab.pdfCurrent(), true);
  assert.equal(f.tab.fitting(), false);
  f.destroy();
});

test('saving keeps the PDF visible and switching jobs clears it', async () => {
  const pending = deferred();
  const f = fixture({ GET: async () => ok(preview(1)), PUT: () => pending.promise });
  await f.tab.load('job');
  const previous = f.tab.fitted();
  const saving = f.tab.save({ page_budget: 2 });
  assert.equal(f.tab.fitted(), previous);
  assert.equal(f.tab.pdfCurrent(), false);
  pending.resolve(ok({ ...preview(2), draft: { ...preview(2).draft, page_budget: 2 } }));
  await saving;
  assert.equal(f.tab.pdfCurrent(), true);
  f.tab.jobId.set('another-job');
  f.effects[0]();
  assert.equal(f.tab.fitted(), undefined);
  assert.equal(f.tab.pdfCurrent(), false);
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
  f.tab.preview.set = (next) => {
    versions.push(next.draft.version);
    set(next);
  };
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
  const f = fixture({
    GET: async () => ok(p),
    PUT: async () => {
      saves++;
      return ok(p);
    },
  });
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
  f.tab.preview.set({
    ...preview(1),
    summary_status: 'failed',
    summary_reason: 'result_unavailable',
  });
  assert.equal(f.tab.summaryGenerating(), false);
  assert.match(f.tab.summaryMessage(), /Try Regenerate/);
  f.tab.preview.set({ ...preview(1), summary_status: 'blocked', summary_reason: 'usage_limit' });
  assert.equal(f.tab.summaryGenerating(), false);
  assert.match(f.tab.summaryMessage(), /allowance/);
  f.destroy();
});

test('summary failures distinguish length validation from empty model output', () => {
  const f = fixture({});
  f.tab.preview.set({
    ...preview(1),
    summary_status: 'failed',
    summary_reason: 'summary_character_limit',
  });
  assert.match(f.tab.summaryMessage(), /exceeded the configured length/);
  f.tab.preview.set({
    ...preview(1),
    summary_status: 'failed',
    summary_reason: 'summary_empty_output',
  });
  assert.match(f.tab.summaryMessage(), /returned no summary text/);
});

test('export sends the exact backfilled omissions and downloads the same fitted snapshot', async () => {
  const c = {
    ...content,
    sections: [
      {
        key: 'experience',
        entries: [
          { id: 'small', title: 'Older role' },
          { id: 'large', title: 'Long role' },
          { id: 'strong', title: 'Best match' },
        ],
      },
    ],
  };
  const p = { ...preview(1), content: c, trim: ['small', 'large', 'strong'] };
  let exported;
  let downloaded;
  const helper = transpile('cv-content.ts', () => ({}));
  const f = fixture(
    {
      GET: async (path) => ok(path.endsWith('/exports') ? { items: [] } : p),
      POST: async (_path, request) => {
        exported = request.body;
        return ok({ content: helper.applyDrops(c, exported.dropped) });
      },
    },
    async (_template, c) => {
      const size = c.sections
        .flatMap((s) => s.entries)
        .reduce((n, e) => n + { small: 3, large: 7, strong: 7 }[e.id], 0);
      return {
        pages: Math.max(1, Math.ceil(size / 10)),
        pdf: new Uint8Array(Buffer.from(JSON.stringify(c))),
      };
    },
  );
  f.tab.downloadContent = async (_template, c) => {
    downloaded = c;
  };
  await f.tab.load('job');
  assert.equal(f.tab.error(), '');
  assert.ok(f.tab.fitted());
  assert.deepEqual(Array.from(f.tab.dropped()), ['large']);
  await f.tab.export();
  assert.deepEqual(Array.from(exported.dropped), ['large']);
  assert.equal(exported.pages, 1);
  assert.equal(
    exported.client_sha256,
    require('node:crypto').createHash('sha256').update(f.tab.fitted().pdf).digest('hex'),
  );
  assert.deepEqual(
    JSON.parse(JSON.stringify(downloaded)),
    JSON.parse(JSON.stringify(helper.applyDrops(c, ['large']))),
  );
  f.destroy();
});

test('unsaved summary text blocks export and saving it restores export readiness', async () => {
  let posts = 0;
  const p = preview(1);
  const f = fixture({
    GET: async () => ok(p),
    PUT: async (_path, request) =>
      ok({ ...preview(2), draft: { ...p.draft, version: 2, summary: request.body.summary } }),
    POST: async () => {
      posts++;
      return ok({ content });
    },
  });
  await f.tab.load('job');
  f.tab.summaryText = 'My edited summary';
  assert.equal(f.tab.hasSummaryEdits, true);
  await f.tab.export();
  assert.equal(posts, 0);
  await f.tab.save({ summary: f.tab.summaryText });
  assert.equal(f.tab.hasSummaryEdits, false);
  assert.equal(f.tab.summaryText, 'My edited summary');
  f.destroy();
});
