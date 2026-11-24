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
const { queryHarness } = require('./query-harness.cjs');
let queries = queryHarness();
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
const ChangeDetectorRef = Symbol('ChangeDetectorRef');
const toast = { error() {}, success() {}, warning() {} };
const core = {
  signal,
  computed: (f) => f,
  effect: () => ({ destroy() {} }),
  untracked: (f) => f(),
  input: signal,
  model: signal,
  output: () => ({ emit() {} }),
  viewChild: () => () => undefined,
  DestroyRef,
  ChangeDetectorRef,
  inject(token) {
    if (token === AuthService) return currentAuth;
    if (token === queries.QueryClientToken) return queries.client;
    if (token === Api) return { client: apiClient };
    if (token === EventsService) return currentEvents;
    if (token === JobsStoreToken) return currentStore;
    if (token === ZardSonnerService) return toast;
    if (token === ChangeDetectorRef) return { markForCheck() {} };
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
    if (name === '@tanstack/angular-query') return queries.tanstack;
    if (name.endsWith('/auth.service')) return { AuthService };
    if (name.endsWith('/api'))
      return { Api, etagOf: (r) => r.response.headers.get('ETag'), ApiError: Error };
    if (name.endsWith('/events.service')) return { EventsService };
    if (name.endsWith('/jobs.store'))
      return {
        JobsStore: JobsStoreToken,
        jobActivitiesKey: (identity, id) => ['job-activities', identity, id],
      };
    if (name.endsWith('/confirm.service')) return { ConfirmService };
    // Plain data and parsing helpers: the sheet builds its options from them.
    if (name === './format') return load('web/src/app/jobs/format.ts');
    if (name === './posting-link') return load('web/src/app/jobs/posting-link.ts');
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
  queries.client.clear();
  currentAuth.identity.set('alice');
  currentEvents = events();
  apiClient = client;
  return new JobsStore();
}
const until = async (check) => {
  for (let i = 0; i < 100 && !check(); i++) await new Promise((r) => setTimeout(r, 5));
  assert.ok(check(), 'condition not reached');
};
/** Loads the board and waits for its first list. */
async function loaded(store) {
  store.ensureLoaded();
  await until(() => !store.loading());
}

test('each member reads their own cached board and a late old-account list stays theirs', async () => {
  const late = deferred();
  const store = setup({
    GET: async (path) =>
      path === '/v1/jobs'
        ? currentAuth.identity() === 'alice'
          ? late.promise
          : ok({ items: [job(1, 'bob')] })
        : ok([{ id: currentAuth.identity() }]),
  });
  store.ensureLoaded();
  store.jobs();
  currentAuth.identity.set('bob');
  await until(() => store.jobs().length === 1 && store.statuses().length && store.views().length);
  late.resolve(ok({ items: [job(9, 'alice')] }));
  await tick();
  assert.equal(
    store
      .jobs()
      .map((j) => j.id)
      .join(),
    'bob',
  );
  assert.equal(store.statuses()[0].id, 'bob');
  assert.equal(store.views()[0].id, 'bob');
  currentAuth.identity.set(null);
  assert.equal(store.jobs().length, 0);
  assert.equal(store.statuses().length, 0);
  assert.equal(store.views().length, 0);
});

test('a move shows at once, a late list cannot undo it, and a deletion removes the job', async () => {
  let list = Promise.resolve(ok({ items: [job(1)] }));
  const moved = deferred();
  const store = setup({
    GET: async (path) => (path === '/v1/jobs' ? list : ok([])),
    POST: async () => moved.promise,
  });
  await loaded(store);
  const late = deferred();
  list = late.promise;
  currentEvents.changes.next({ topic: 'job.updated', data: { id: 'j', version: 2 } });
  await tick();
  const move = store.move('j', 'active');
  await until(() => store.jobs()[0].status_id === 'active');
  late.resolve(ok({ items: [job(2)] }));
  await tick();
  assert.equal(store.jobs()[0].status_id, 'active');
  moved.resolve(ok({ ...job(4), status_id: 'active' }));
  await move;
  assert.equal(store.jobs()[0].version, 4);
  currentEvents.changes.next({ topic: 'job.deleted', data: { id: 'j' } });
  await until(() => store.jobs().length === 0);
});

test('an event during paging wins over the late list it replaces', async () => {
  const stale = deferred();
  let lists = 0;
  const store = setup({
    GET: async (path) =>
      path === '/v1/jobs' ? (++lists === 1 ? stale.promise : ok({ items: [job(3)] })) : ok([]),
  });
  store.ensureLoaded();
  store.jobs();
  await tick();
  currentEvents.changes.next({ topic: 'job.updated', data: { id: 'j', version: 3 } });
  await until(() => store.jobs()[0]?.version === 3);
  stale.resolve(ok({ items: [job(1)] }));
  await tick();
  assert.equal(store.jobs()[0].version, 3);
});

test('older mutation responses cannot replace a newer copy of the job', async () => {
  let items = [job(1)];
  const responses = [];
  const store = setup({
    GET: async (path) => (path === '/v1/jobs' ? ok({ items }) : ok([])),
    POST: async () => ok(responses.shift()),
  });
  await loaded(store);
  items = [job(5)];
  currentEvents.changes.next({ topic: 'job.updated', data: { id: 'j', version: 5 } });
  await until(() => store.jobs()[0]?.version === 5);
  responses.push({ ...job(4), status_id: 'active' });
  await store.move('j', 'active');
  assert.equal(store.jobs()[0].version, 5);
  responses.push({ ...job(4), archived: true });
  await store.setArchived('j', true);
  assert.equal(store.jobs().length, 1, 'an older archive response leaves the newer job listed');
  responses.push({ ...job(6), archived: true });
  await store.setArchived('j', true);
  assert.equal(store.jobs().length, 0, 'a newer archive removes it from the unarchived list');
});

test('a late mutation response cannot resurrect a deleted job', async () => {
  const moved = deferred();
  const store = setup({
    GET: async (path) => (path === '/v1/jobs' ? ok({ items: [job(1)] }) : ok([])),
    POST: async () => moved.promise,
  });
  await loaded(store);
  const move = store.move('j', 'active');
  await tick();
  currentEvents.changes.next({ topic: 'job.deleted', data: { id: 'j' } });
  await until(() => store.jobs().length === 0);
  moved.resolve(ok({ ...job(10), status_id: 'active' }));
  await move;
  assert.equal(store.jobs().length, 0);
});

test('deletion during paging cannot be resurrected by a late list', async () => {
  const stale = deferred();
  let lists = 0;
  const store = setup({
    GET: async (path) =>
      path === '/v1/jobs' ? (++lists === 1 ? stale.promise : ok({ items: [] })) : ok([]),
  });
  store.ensureLoaded();
  store.jobs();
  await tick();
  currentEvents.changes.next({ topic: 'job.deleted', data: { id: 'j' } });
  await tick();
  stale.resolve(ok({ items: [job(1)] }));
  await until(() => !store.loading());
  await tick();
  assert.equal(store.jobs().length, 0);
});

const { JobSheet } = load('web/src/app/jobs/job-sheet.ts');
function sheetStore(overrides = {}) {
  currentStore = {
    jobs: signal([]),
    statuses: signal([]),
    cached: () => undefined,
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
  let items = [job(3)];
  const store = setup({ GET: async (path) => ok(path === '/v1/jobs' ? { items } : []) });
  await loaded(store);
  assert.equal(store.jobs().length, 1);
  items = [];
  await queries.client.invalidateQueries(); // what core/query.ts does on resync
  await until(() => store.jobs().length === 0);
});

test('database tabs select views without opening settings and keep layout controlled', () => {
  const { Database } = load('web/src/app/shared/database.ts');
  const database = new Database();
  database.views.set([{ id: 'interviews', name: 'Interviews', layout: 'table' }]);
  database.activeViewId.set('interviews');
  database.layout.set('table');
  const selections = [];
  database.viewChange = { emit: (id) => selections.push(id) };
  database.select('interviews');
  assert.equal(database.activeView().name, 'Interviews');
  assert.equal(database.viewSettingsOpen(), false);
  database.select(null);
  assert.deepEqual(selections, ['interviews', null]);
  assert.equal(database.viewSettingsOpen(), false);
  assert.equal(database.layout(), 'table');
});

test('new job capture defaults to a saved stage and preserves a board column choice', async () => {
  const sheet = sheetStore({
    statuses: signal([
      { id: 'interview', stage: 'active' },
      { id: 'bookmarked', stage: 'saved' },
    ]),
  });
  await sheet.load({ new: true });
  assert.equal(sheet.form.status_id, 'bookmarked');
  await sheet.load({ new: true, statusId: 'interview' });
  assert.equal(sheet.form.status_id, 'interview');
  await sheet.changeStatus('bookmarked');
  assert.equal(sheet.dirty(), true);
});

test('capture rejects blank titles, saves minimal input once, and opens the saved job', async () => {
  const pending = deferred();
  const inputs = [],
    opened = [];
  const sheet = sheetStore({
    create: async (input) => {
      inputs.push(input);
      return pending.promise;
    },
  });
  sheet.opened = { emit: (id) => opened.push(id) };
  await sheet.load({ new: true });
  sheet.form.title = '   ';
  await sheet.save();
  assert.equal(inputs.length, 0);
  assert.equal(sheet.errors().title, 'Enter a job title.');
  sheet.form.title = '  Platform engineer  ';
  const saving = sheet.save();
  await sheet.save();
  assert.equal(inputs.length, 1);
  assert.equal(inputs[0].title, 'Platform engineer');
  pending.resolve(job(1, 'created'));
  await saving;
  assert.deepEqual(opened, ['created']);
  assert.equal(sheet.saving(), false);
});

test('a pasted link reads behind a loader and opens a form with the extraction already in it', async () => {
  const page = deferred();
  const sheet = sheetStore({
    readPosting: () => page.promise,
    postingExtraction: async () => ({
      status: 'completed',
      title: 'ML Engineer',
      salary_min: 150400,
      salary_max: 277600,
      salary_currency: 'USD',
      salary_period: 'year',
    }),
  });
  await sheet.load({ new: true });
  sheet.form.url = 'https://jobs.example.com/en-us/details/1/ml-engineer';
  const reading = sheet.continueFromLink();
  assert.equal(sheet.step(), 'reading');
  assert.equal(sheet.reading(), true);
  page.resolve({ description: 'Raw page text', extraction_id: 'x1' });
  await until(() => sheet.extracting());
  assert.equal(sheet.step(), 'reading', 'the loader stays up while the model reads');
  await reading;
  assert.equal(sheet.step(), 'review');
  assert.equal(sheet.form.title, 'ML Engineer');
  assert.equal(sheet.form.salary_min, 150400);
  assert.equal(sheet.form.salary_max, 277600);
  assert.equal(sheet.form.salary_currency, 'USD');
  assert.equal(sheet.filled().has('salary_min'), true);
});

test('skipping the loader opens the form now and the extraction still lands in it', async () => {
  const extraction = deferred();
  const sheet = sheetStore({
    readPosting: async () => ({ description: 'Raw page text', extraction_id: 'x1' }),
    postingExtraction: () => extraction.promise,
  });
  await sheet.load({ new: true });
  sheet.form.url = 'https://jobs.example.com/1';
  const reading = sheet.continueFromLink();
  await until(() => sheet.extracting());
  sheet.skipWaiting();
  assert.equal(sheet.step(), 'review');
  assert.equal(
    sheet.form.title,
    'Raw page text',
    'until the model answers, the page text gives a guess',
  );
  sheet.step.set('link'); // back to the link while the model is still reading
  extraction.resolve({ status: 'completed', title: 'Late title' });
  await reading;
  await until(() => sheet.form.title === 'Late title');
  assert.equal(sheet.extracting(), false);
  assert.equal(sheet.step(), 'link', 'a late answer does not pull the person off the link');
});

test('cancelling the loader returns to the link and drops the late answer', async () => {
  const page = deferred();
  const sheet = sheetStore({ readPosting: () => page.promise });
  await sheet.load({ new: true });
  sheet.form.url = 'https://jobs.example.com/1';
  const reading = sheet.continueFromLink();
  sheet.cancelReading();
  assert.equal(sheet.step(), 'link');
  assert.equal(sheet.reading(), false);
  page.resolve({ description: 'Raw page text', title: 'Late title' });
  await reading;
  assert.equal(sheet.step(), 'link');
  assert.equal(sheet.form.title, '');
  assert.equal(sheet.form.description, '');
});

test('a failed extraction still opens the form, with a note', async () => {
  const sheet = sheetStore({
    readPosting: async () => ({ description: 'Raw page text', extraction_id: 'x1' }),
    postingExtraction: async () => ({ status: 'failed' }),
  });
  await sheet.load({ new: true });
  sheet.form.url = 'https://jobs.example.com/1';
  await sheet.continueFromLink();
  assert.equal(sheet.step(), 'review');
  assert.match(sheet.readProblem(), /Couldn’t read the details/);
  assert.equal(sheet.form.description, 'Raw page text');
});

test('when the page cannot be read, the form opens for a pasted description that is then read', async () => {
  const calls = [];
  const sheet = sheetStore({
    readPosting: async (url, text) => {
      calls.push(text);
      if (!text) throw Object.assign(new Error('blocked'), { status: 502, fieldErrors: [] });
      return { description: '', extraction_id: 'x2' };
    },
    postingExtraction: async () => ({ status: 'completed', title: 'Pasted role', company: 'Acme' }),
  });
  await sheet.load({ new: true });
  sheet.form.url = 'https://jobs.example.com/1';
  await sheet.continueFromLink();
  assert.equal(sheet.step(), 'review');
  assert.match(sheet.readProblem(), /Couldn’t read the page/);
  assert.equal(sheet.descriptionOpen(), true, 'the description is open to paste into');
  sheet.form.description = 'Acme is hiring a role. '.repeat(20);
  await sheet.readPastedText();
  await until(() => sheet.step() === 'review' && sheet.form.company === 'Acme');
  assert.equal(calls.length, 2);
  assert.match(calls[1], /Acme is hiring/);
  assert.equal(sheet.form.title, 'Pasted role');
  assert.equal(sheet.readProblem(), '');
});

test('closing capture preserves a rejected draft and restores the dialog after Escape', async () => {
  const sheet = sheetStore();
  await sheet.load({ new: true });
  sheet.visible.set(true);
  sheet.form.title = 'Draft';
  sheet.touch();
  sheet.confirm = { ask: async () => false };
  const dialog = { zVisible: signal(false) };
  await sheet.captureVisibility(false, dialog);
  assert.equal(sheet.visible(), true);
  assert.equal(dialog.zVisible(), true);
  assert.equal(sheet.form.title, 'Draft');
  sheet.confirm = { ask: async () => true };
  await sheet.close();
  assert.equal(sheet.visible(), false);
});

test('capture cannot close while its save is in flight', async () => {
  const pending = deferred();
  const sheet = sheetStore({ create: () => pending.promise });
  await sheet.load({ new: true });
  sheet.visible.set(true);
  sheet.form.title = 'Draft';
  const saving = sheet.save();
  const dialog = { zVisible: signal(false) };
  await sheet.captureVisibility(false, dialog);
  assert.equal(sheet.visible(), true);
  assert.equal(dialog.zVisible(), true);
  pending.resolve(job());
  await saving;
});

test('job tab URLs preserve the saved view and clear when switching or closing a job', () => {
  currentStore = { ensureLoaded: async () => {}, views: signal([]) };
  const { JobsPage } = load('web/src/app/jobs/jobs.page.ts');
  const page = new JobsPage();
  const calls = [];
  page.router = {
    navigate: (_, options) => {
      calls.push(options);
      return Promise.resolve(true);
    },
  };
  page.changeTab('cv');
  page.changeTab('details');
  page.open('another-job');
  page.sheetVisible(false);
  assert.deepEqual(JSON.parse(JSON.stringify(calls)), [
    { queryParams: { tab: 'cv' }, queryParamsHandling: 'merge' },
    { queryParams: { tab: null }, queryParamsHandling: 'merge' },
    { queryParams: { job: 'another-job', tab: null }, queryParamsHandling: 'merge' },
    { queryParams: { job: null, tab: null, url: null }, queryParamsHandling: 'merge' },
  ]);
});
