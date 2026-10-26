// Executes production class methods with controlled DI and signals, without a DOM.
// Deferred responses make request ordering deterministic; Angular templates are
// separately checked by the production build. No source assertions are used.
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const root = require('node:path').resolve(__dirname, '../..');
const test = require('node:test');
const ts = require(root + '/web/node_modules/typescript');
const { Subject } = require(root + '/web/node_modules/rxjs');
function signal(v) {
  const s = () => v;
  s.set = (x) => (v = x);
  s.update = (f) => (v = f(v));
  return s;
}
signal.required = () => signal(undefined);
let apiClient, currentEvents, currentStore;
const currentAuth = { identity: signal('alice') };
class AuthService {}
class Api {
  static ok(r) {
    if (!r.response.ok) throw new Error('failed');
    return r.data;
  }
}
class EventsService {}
class JobsStoreToken {}
class ZardSonnerService {}
class ConfirmService {}
const DestroyRef = Symbol('DestroyRef');
const toast = { error() {}, success() {}, warning() {} };
const core = {
  signal,
  computed: (f) => f,
  effect: () => ({ destroy() {} }),
  untracked: (f) => f(),
  input: signal,
  model: signal,
  output: () => ({ emit() {} }),
  DestroyRef,
  inject(token) {
    if (token === AuthService) return currentAuth;
    if (token === Api) return { client: apiClient };
    if (token === EventsService) return currentEvents;
    if (token === JobsStoreToken) return currentStore;
    if (token === ZardSonnerService) return toast;
    return {};
  },
  Injectable: () => (x) => x,
  ChangeDetectionStrategy: { OnPush: 1 },
};
function load(file, names) {
  // Drop only component decorator metadata, retaining the actual class code.
  let src = fs.readFileSync(root + '/' + file, 'utf8');
  if (src.includes('@Component(')) {
    const start = src.indexOf('@Component('),
      end = src.indexOf('export class', start);
    src = src.slice(0, start) + src.slice(end);
  }
  const out = ts.transpileModule(src, {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.CommonJS,
      experimentalDecorators: true,
    },
  }).outputText;
  const module = { exports: {} };
  const stub = new Proxy(
    {},
    {
      get: (_, key) =>
        key.endsWith('Imports') ? [] : key === 'ZardSonnerService' ? ZardSonnerService : () => {},
    },
  );
  const requireStub = (name) => {
    if (name === '@angular/core') return core;
    if (name === '@angular/core/rxjs-interop') return { takeUntilDestroyed: () => (s) => s };
    if (name === 'rxjs') return { Subject };
    if (name.endsWith('/auth.service')) return { AuthService };
    if (name.endsWith('/api'))
      return { Api, etagOf: (r) => r.response.headers.get('ETag'), ApiError: Error };
    if (name.endsWith('/events.service')) return { EventsService };
    if (name.endsWith('/jobs.store')) return { JobsStore: JobsStoreToken };
    if (name.endsWith('/confirm.service')) return { ConfirmService };
    return stub;
  };
  vm.runInNewContext(
    out,
    {
      module,
      exports: module.exports,
      require: requireStub,
      console,
      Map,
      Set,
      Promise,
      Date,
      Number,
      fetch: (...args) => globalThis.fetch(...args),
      TextDecoderStream,
      setTimeout,
      clearTimeout,
      AbortController,
    },
    { filename: file },
  );
  return module.exports;
}
const { JobsStore } = load('web/src/app/jobs/jobs.store.ts');
const ok = (data) => ({ data, response: { ok: true, status: 200, headers: { get: () => '"1"' } } });
const deferred = () => {
  let resolve;
  const promise = new Promise((r) => (resolve = r));
  return { promise, resolve };
};
const events = () => ({ changes: new Subject(), resync: new Subject() });
const job = (version = 1, id = 'j') => ({
  id,
  version,
  status_id: 'saved',
  position: 'V',
  title: id,
  company: '',
  notes: 'original',
  description: 'original',
  archived: false,
});

const tick = () => new Promise((r) => setImmediate(r));
function setup(client) {
  currentAuth.identity.set('alice');
  currentEvents = events();
  apiClient = client;
  return new JobsStore();
}

test('account switch clears all caches and rejects prior-account responses', async () => {
  let account = 'alice';
  const pending = deferred();
  let delayed = false;
  const store = setup({
    GET: async (path) =>
      path === '/v1/jobs'
        ? delayed
          ? pending.promise
          : ok({ items: [job(1, account)] })
        : ok([{ id: account }]),
  });
  await store.ensureLoaded();
  assert.equal(store.jobs()[0].id, 'alice');
  delayed = true;
  const old = store.load();
  currentAuth.identity.set('bob');
  account = 'bob';
  delayed = false;
  await store.ensureLoaded();
  assert.equal(store.jobs()[0].id, 'bob');
  pending.resolve(ok({ items: [job(9, 'alice')] }));
  await old;
  assert.equal(store.jobs().length, 1);
  assert.equal(store.jobs()[0].id, 'bob');
  assert.equal(store.statuses()[0].id, 'bob');
  assert.equal(store.views()[0].id, 'bob');
  currentAuth.identity.set(null);
  store.syncSession(); // the identity effect invokes this at sign-out
  assert.equal(store.jobs().length, 0);
  assert.equal(store.statuses().length, 0);
  assert.equal(store.views().length, 0);
});

test('late lists and moves preserve newer events and deleted jobs', async () => {
  const list = deferred(),
    moved = deferred();
  const store = setup({
    GET: async (path) =>
      path === '/v1/jobs' ? list.promise : path === '/v1/jobs/{id}' ? ok(job(3)) : ok([]),
    POST: async () => moved.promise,
  });
  const loading = store.load();
  currentEvents.changes.next({ topic: 'job.updated', data: { id: 'j', version: 3 } });
  await tick();
  list.resolve(ok({ items: [job(1)] }));
  await loading;
  assert.equal(store.jobs()[0].version, 3);
  const move = store.move('j', 'active');
  store.upsert(job(5));
  moved.resolve(ok(job(4)));
  await move;
  assert.equal(store.jobs()[0].version, 5);
  store.upsert({ ...job(4), archived: true });
  assert.equal(store.jobs()[0].version, 5);
  store.upsert({ ...job(6), archived: true });
  store.upsert(job(5));
  assert.equal(store.jobs().length, 0);
  currentEvents.changes.next({ topic: 'job.deleted', data: { id: 'j' } });
  store.upsert(job(10));
  assert.equal(store.jobs().length, 0);
});

test('deletion during paging cannot be resurrected by a late list', async () => {
  const pending = deferred();
  const store = setup({ GET: async (path) => (path === '/v1/jobs' ? pending.promise : ok([])) });
  const loading = store.load();
  currentEvents.changes.next({ topic: 'job.deleted', data: { id: 'j' } });
  pending.resolve(ok({ items: [job(1)] }));
  await loading;
  assert.equal(store.jobs().length, 0);
});

const { JobSheet } = load('web/src/app/jobs/job-sheet.ts');
function sheetStore(overrides = {}) {
  currentStore = {
    jobs: signal([]),
    statuses: signal([]),
    activityChanged: new Subject(),
    activities: async () => [],
    ...overrides,
  };
  return new JobSheet();
}

test('job sheet ignores superseded job and activity responses', async () => {
  const a = deferred(),
    b = deferred(),
    activity = deferred();
  const sheet = sheetStore({
    get: (id) => (id === 'A' ? a.promise : b.promise),
    activities: async (id) => (id === 'B' ? activity.promise : []),
  });
  const la = sheet.load({ id: 'A' }),
    lb = sheet.load({ id: 'B' });
  b.resolve({ job: job(1, 'B'), etag: '"1"' });
  await tick();
  a.resolve({ job: job(1, 'A'), etag: '"1"' });
  await la;
  assert.equal(sheet.job().id, 'B');
  await sheet.load({ new: true });
  activity.resolve([{ id: 'B-note' }]);
  await lb;
  assert.equal(sheet.job(), null);
  assert.equal(sheet.activities().length, 0);
});

test('status and archive actions preserve draft fields and advance the ETag', async () => {
  let current = job(1, 'A');
  const sheet = sheetStore({
    get: async () => ({ job: current, etag: '"1"' }),
    move: async () =>
      (current = { ...current, version: 2, status_id: 'active', applied_on: '2026-10-26' }),
    setArchived: async () => (current = { ...current, version: 3, archived: true }),
  });
  await sheet.load({ id: 'A' });
  sheet.form.notes = 'draft';
  sheet.dirty.set(true);
  await sheet.changeStatus('active');
  assert.equal(sheet.form.notes, 'draft');
  assert.equal(sheet.dirty(), true);
  assert.equal(sheet.form.status_id, 'active');
  assert.equal(sheet.etag, '"2"');
  assert.equal(sheet.form.applied_on, '2026-10-26');
  await sheet.archive(true);
  assert.equal(sheet.form.notes, 'draft');
  assert.equal(sheet.dirty(), true);
  assert.equal(sheet.job().archived, true);
  assert.equal(sheet.etag, '"3"');
});

test('a late status action cannot change a newly selected job', async () => {
  const moved = deferred();
  const sheet = sheetStore({
    get: async (id) => ({ job: job(1, id), etag: '"1"' }),
    move: async () => moved.promise,
  });
  await sheet.load({ id: 'A' });
  const moving = sheet.changeStatus('active');
  await sheet.load({ id: 'B' });
  moved.resolve({ ...job(2, 'A'), status_id: 'active' });
  await moving;
  assert.equal(sheet.job().id, 'B');
});

test('saved views ignore removed status IDs for both columns and jobs', () => {
  currentStore = {
    ensureLoaded: async () => {},
    jobs: signal([job(1)]),
    statuses: signal([{ id: 'saved', stage: 'saved' }]),
    statusById: () => new Map([['saved', {}]]),
    views: signal([]),
  };
  const { JobsPage } = load('web/src/app/jobs/jobs.page.ts');
  const page = new JobsPage();
  page.state.set({ layout: 'board', filters: { status_ids: ['removed'] } });
  assert.equal(page.columns().length, 1);
  assert.equal(page.filtered().length, 1);
  page.state.set({ layout: 'board', filters: { status_ids: ['removed', 'saved'], q: 'absent' } });
  assert.equal(page.columns().length, 1);
  assert.equal(page.filtered().length, 0);
});

test('resync removes jobs absent from a fresh list after missed events', async () => {
  const store = setup({ GET: async (path) => ok(path === '/v1/jobs' ? { items: [] } : []) });
  store.upsert(job(3));
  await store.load();
  assert.equal(store.jobs().length, 0);
});
