// Production class methods with real Angular DI, signals and root effects.
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const test = require('node:test');
const ts = require('typescript');
const { Subject } = require('rxjs');
class AuthService {}
class EventsService {}
class StoreToken {}
class Toast {}
class Confirm {}
class ApiError extends Error {
  constructor(status) {
    super('request failed');
    this.status = status;
    this.fieldErrors = [];
  }
}
class Api {
  static ok(r) {
    if (!r.response.ok) throw new ApiError(r.response.status);
    return r.data;
  }
}
const ok = (data) => ({ data, response: { ok: true, headers: { get: () => '"1"' } } });
const tick = () => new Promise((r) => setImmediate(r));
const job = (id, version = 1) => ({
  id,
  version,
  status_id: 'saved',
  position: 'V',
  title: id,
  company: '',
  url: '',
  location: '',
  source: '',
  salary_currency: '',
  notes: 'private notes',
  description: 'private description',
  archived: false,
});
function load(file, core) {
  let src = fs.readFileSync(path.resolve(__dirname, '../src/app', file), 'utf8');
  if (src.includes('@Component(')) {
    const start = src.indexOf('@Component(');
    src = src.slice(0, start) + src.slice(src.indexOf('export class', start));
  }
  const code = ts.transpileModule(src, {
    compilerOptions: {
      module: ts.ModuleKind.CommonJS,
      target: ts.ScriptTarget.ES2022,
      experimentalDecorators: true,
    },
  }).outputText;
  const module = { exports: {} };
  const other = new Proxy({}, { get: () => () => {} });
  const requireStub = (name) => {
    if (name === '@angular/core') return core;
    if (name === '@angular/core/rxjs-interop') return { takeUntilDestroyed: () => (s) => s };
    if (name === 'rxjs') return { Subject };
    if (name.endsWith('/auth.service')) return { AuthService };
    if (name.endsWith('/events.service')) return { EventsService };
    if (name.endsWith('/jobs.store')) return { JobsStore: StoreToken };
    if (name.endsWith('/api'))
      return { Api, ApiError, etagOf: (r) => r.response.headers.get('ETag') };
    if (name.endsWith('/sonner')) return { ZardSonnerService: Toast };
    if (name.endsWith('/confirm.service')) return { ConfirmService: Confirm };
    return other;
  };
  vm.runInNewContext(code, {
    module,
    exports: module.exports,
    require: requireStub,
    console,
    Error,
    Map,
    Set,
    Promise,
  });
  return module.exports;
}
async function fixture(t, overrides = {}) {
  const core = await import('@angular/core');
  const pending = new Set();
  const scheduler = {
    add: (e) => pending.add(e),
    schedule: (e) => pending.add(e),
    remove: (e) => pending.delete(e),
  };
  const flush = () => {
    for (let count = 0; pending.size; count++) {
      if (count > 100) throw Error('effect loop');
      const batch = [...pending];
      pending.clear();
      for (const e of batch) e.run();
    }
  };
  const auth = { identity: core.signal('alice') };
  const events = { changes: new Subject(), resync: new Subject() };
  let requests = 0;
  const client = {
    GET: async (p) => {
      requests++;
      return ok(p === '/v1/jobs' ? { items: [job(auth.identity())] } : []);
    },
  };
  const injector = core.createEnvironmentInjector([
    { provide: core.ɵChangeDetectionScheduler, useValue: { notify() {} } },
    { provide: core.ɵEffectScheduler, useValue: scheduler },
    { provide: AuthService, useValue: auth },
    { provide: EventsService, useValue: events },
    { provide: Api, useValue: { client } },
    { provide: Toast, useValue: { error() {}, success() {}, warning() {} } },
    { provide: Confirm, useValue: {} },
    {
      provide: StoreToken,
      useValue: {
        jobs: core.signal([]),
        statuses: core.signal([]),
        activityChanged: new Subject(),
        get: async (id) => ({ job: job(id), etag: '"1"' }),
        activities: async () => [],
        ...overrides,
      },
    },
  ]);
  t.after(() => injector.destroy());
  return {
    core,
    auth,
    events,
    flush,
    requests: () => requests,
    create: (file, name) =>
      core.runInInjectionContext(injector, () => new (load(file, core)[name])()),
  };
}
test('mounted board reloads for a new principal without remount or SSE', async (t) => {
  const f = await fixture(t);
  const store = f.create('jobs/jobs.store.ts', 'JobsStore');
  f.flush();
  await store.ensureLoaded();
  f.flush();
  assert.equal(store.jobs()[0].id, 'alice');
  const before = f.requests();
  f.auth.identity.set('bob');
  f.flush();
  await tick();
  f.flush();
  assert.equal(store.jobs()[0].id, 'bob');
  assert.ok(f.requests() > before);
  f.auth.identity.set(null);
  f.flush();
  assert.equal(store.jobs().length, 0);
  assert.equal(store.statuses().length, 0);
});
test('mounted sheet clears private data and invalidates pending loads on principal change', async (t) => {
  let resolve;
  const late = new Promise((r) => (resolve = r));
  const f = await fixture(t, {
    get: (id) => (id === 'late' ? late : Promise.resolve({ job: job(id), etag: '"1"' })),
  });
  const sheet = f.create('jobs/job-sheet.ts', 'JobSheet');
  f.flush();
  await sheet.load({ id: 'alice' });
  f.flush();
  sheet.form.notes = 'private draft';
  sheet.dirty.set(true);
  sheet.note = 'private activity';
  sheet.visible.set(true);
  const loading = sheet.load({ id: 'late' }, true);
  f.auth.identity.set('bob');
  f.flush();
  assert.equal(sheet.job(), null);
  assert.equal(sheet.form.notes, '');
  assert.equal(sheet.note, '');
  assert.equal(sheet.activities().length, 0);
  assert.equal(sheet.visible(), false);
  resolve({ job: job('late'), etag: '"2"' });
  await loading;
  f.flush();
  assert.equal(sheet.job(), null);
});
test('412 recovery releases Save and preserves edits with the new ETag', async (t) => {
  const f = await fixture(t, {
    get: async (id) => ({ job: job(id, 2), etag: '"2"' }),
    save: async () => {
      throw new ApiError(412);
    },
  });
  const sheet = f.create('jobs/job-sheet.ts', 'JobSheet');
  f.flush();
  await sheet.load({ id: 'alice' });
  f.flush();
  sheet.form.notes = 'draft';
  sheet.dirty.set(true);
  await sheet.save();
  await tick();
  f.flush();
  assert.equal(sheet.saving(), false);
  assert.equal(sheet.dirty(), true);
  assert.equal(sheet.form.notes, 'draft');
  assert.equal(sheet.etag, '"2"');
});

test('workspace tabs reset for new jobs and accounts while quiet refreshes preserve visited panels', async (t) => {
  const f = await fixture(t);
  const sheet = f.create('jobs/job-sheet.ts', 'JobSheet');
  f.flush();
  await sheet.load({ id: 'first' });
  assert.equal(sheet.activeTab(), 'details');
  sheet.selectTab('match');
  await sheet.load({ id: 'first' }, true);
  assert.equal(sheet.activeTab(), 'match');
  await sheet.load({ id: 'second' });
  assert.equal(sheet.activeTab(), 'details');
  sheet.selectTab('match');
  f.auth.identity.set('bob');
  f.flush();
  assert.equal(sheet.activeTab(), 'details');
});

test('tab links restore the requested panel and history changes retain local drafts', async (t) => {
  const f = await fixture(t);
  const sheet = f.create('jobs/job-sheet.ts', 'JobSheet');
  sheet.tab = f.core.signal('cv');
  f.flush();
  await sheet.load({ id: 'job' });
  f.flush();
  assert.equal(sheet.activeTab(), 'cv');
  assert.equal(sheet.visitedTabs().has('cv'), true);
  sheet.form.notes = 'unsaved details';
  sheet.dirty.set(true);
  sheet.note = 'unsaved follow-up';
  sheet.tab.set('activity');
  f.flush();
  assert.equal(sheet.activeTab(), 'activity');
  assert.equal(sheet.visitedTabs().has('cv'), true);
  assert.equal(sheet.form.notes, 'unsaved details');
  assert.equal(sheet.note, 'unsaved follow-up');
  sheet.tab.set('unknown');
  f.flush();
  assert.equal(sheet.activeTab(), 'details');
  await sheet.load({ new: true });
  sheet.selectTab('cv');
  assert.equal(sheet.activeTab(), 'details');
});

test('keyboard tab navigation wraps, moves focus and leaves ordinary keys alone', async (t) => {
  const f = await fixture(t);
  const sheet = f.create('jobs/job-sheet.ts', 'JobSheet');
  f.flush();
  const focused = [];
  const emitted = [];
  sheet.tabChange.subscribe((tab) => emitted.push(tab));
  const event = (key) => ({
    key,
    preventDefault() {
      this.prevented = true;
    },
    currentTarget: {
      parentElement: {
        querySelectorAll: () => sheet.tabs.map((tab) => ({ focus: () => focused.push(tab.id) })),
      },
    },
  });
  const left = event('ArrowLeft');
  sheet.tabKeydown(left, 'details');
  assert.equal(left.prevented, true);
  assert.equal(sheet.activeTab(), 'activity');
  sheet.tabKeydown(event('ArrowRight'), 'activity');
  assert.equal(sheet.activeTab(), 'details');
  sheet.tabKeydown(event('End'), 'details');
  sheet.tabKeydown(event('Home'), 'activity');
  assert.deepEqual(focused, ['activity', 'details', 'activity', 'details']);
  assert.deepEqual(emitted, focused);
  const enter = event('Enter');
  sheet.tabKeydown(enter, 'details');
  assert.equal(enter.prevented, undefined);
});

test('a timeline error keeps the job open and can be retried', async (t) => {
  let fail = true;
  const f = await fixture(t, {
    activities: async () => {
      if (fail) throw new Error('Timeline unavailable');
      return [{ id: 'restored-note' }];
    },
  });
  const sheet = f.create('jobs/job-sheet.ts', 'JobSheet');
  f.flush();
  sheet.visible.set(true);
  await sheet.load({ id: 'job' });
  assert.equal(sheet.visible(), true);
  assert.equal(sheet.job().id, 'job');
  assert.equal(sheet.activityError(), 'Timeline unavailable');
  assert.equal(sheet.activityLoading(), false);
  fail = false;
  await sheet.loadActivities('job');
  assert.equal(sheet.activityError(), '');
  assert.equal(sheet.activities()[0].id, 'restored-note');
});
