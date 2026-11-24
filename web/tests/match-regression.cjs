// Production MatchStore methods against a real TanStack QueryClient.
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const test = require('node:test');
const ts = require('typescript');
const { Subject } = require('rxjs');
const { queryHarness } = require('./query-harness.cjs');
function signal(value) {
  const s = () => value;
  s.set = (v) => (value = v);
  return s;
}
class AuthService {}
class EventsService {}
class ApiError extends Error {}
class Api {
  static ok(r) {
    if (!r.response.ok) throw new Error('request failed');
    return r.data;
  }
}
const ok = (data) => ({ data, response: { ok: true } });
const deferred = () => {
  let resolve;
  const promise = new Promise((r) => (resolve = r));
  return { promise, resolve };
};
function fixture(client) {
  const auth = { identity: signal('alice') };
  const events = { changes: new Subject(), resync: new Subject() };
  const queries = queryHarness();
  const core = {
    Injectable: () => (x) => x,
    DestroyRef: {},
    inject: (token) =>
      token === AuthService
        ? auth
        : token === EventsService
          ? events
          : token === queries.QueryClientToken
            ? queries.client
            : { client },
  };
  const source = fs.readFileSync(
    require('node:path').resolve(__dirname, '../src/app/jobs/match/match.store.ts'),
    'utf8',
  );
  const code = ts.transpileModule(source, {
    compilerOptions: {
      module: ts.ModuleKind.CommonJS,
      target: ts.ScriptTarget.ES2022,
      experimentalDecorators: true,
    },
  }).outputText;
  const module = { exports: {} };
  vm.runInNewContext(code, {
    module,
    exports: module.exports,
    crypto: require('node:crypto').webcrypto,
    require: (name) =>
      name === '@angular/core'
        ? core
        : name === '@angular/core/rxjs-interop'
          ? { takeUntilDestroyed: () => (s) => s }
          : name === '@tanstack/angular-query'
            ? queries.tanstack
            : name.endsWith('/auth.service')
              ? { AuthService }
              : name.endsWith('/events.service')
                ? { EventsService }
                : { Api, ApiError },
  });
  const { MatchStore, matchRunKey } = module.exports;
  const cache = queries.client;
  const invalid = (key) => cache.getQueryState(key)?.isInvalidated ?? null;
  return { store: new MatchStore(), auth, events, cache, matchRunKey, invalid };
}

test('a job without runs reads as null; otherwise its latest run in full', async () => {
  let items = [];
  const { store } = fixture({
    GET: async (path) => (path.includes('/jobs/') ? ok({ items }) : ok({ id: 'run', version: 2 })),
  });
  assert.equal(await store.fetchRun('job'), null);
  items = [{ id: 'run' }];
  assert.equal((await store.fetchRun('job')).version, 2);
});

test('match runs are cached per member and job', () => {
  const { matchRunKey } = fixture({});
  assert.equal(JSON.stringify(matchRunKey('alice', 'job')), '["match-run","alice","job"]');
  assert.equal(JSON.stringify(matchRunKey('bob')), '["match-run","bob"]');
});

test('correction round trip invalidates the job run', async () => {
  const calls = [];
  const { store, cache, matchRunKey, invalid } = fixture({
    PUT: async (path, req) => (calls.push(req.body.class), ok(null)),
  });
  cache.setQueryData(matchRunKey('alice', 'job'), { id: 'run' });
  await store.correct('job', 'hash', 'ignore');
  assert.deepEqual(calls, ['ignore']);
  assert.equal(invalid(matchRunKey('alice', 'job')), true);
});

test('SSE invalidates only the changed job, and library changes only this member', () => {
  const { events, cache, matchRunKey, invalid } = fixture({});
  for (const key of [
    matchRunKey('alice', 'job'),
    matchRunKey('alice', 'other'),
    matchRunKey('bob', 'job'),
  ])
    cache.setQueryData(key, { id: 'run' });
  events.changes.next({ topic: 'match_run.changed', data: { job_id: 'job' } });
  assert.equal(invalid(matchRunKey('alice', 'job')), true);
  assert.equal(invalid(matchRunKey('alice', 'other')), false);
  events.changes.next({ topic: 'evidence.updated', data: {} });
  assert.equal(invalid(matchRunKey('alice', 'other')), true);
  assert.equal(invalid(matchRunKey('bob', 'job')), false);
});

test('a late action after an account switch only refreshes the member who acted', async () => {
  const pending = deferred();
  const { store, auth, cache, matchRunKey, invalid } = fixture({ POST: () => pending.promise });
  cache.setQueryData(matchRunKey('alice', 'job'), { id: 'alice-run' });
  cache.setQueryData(matchRunKey('bob', 'job'), { id: 'bob-run' });
  const starting = store.start('job');
  auth.identity.set('bob');
  pending.resolve(ok(null));
  await starting;
  assert.equal(invalid(matchRunKey('alice', 'job')), true);
  assert.equal(invalid(matchRunKey('bob', 'job')), false);
  assert.equal(cache.getQueryData(matchRunKey('bob', 'job')).id, 'bob-run');
});

test('newer requests win when an earlier load finishes late', async () => {
  const late = deferred();
  let lists = 0;
  const { store, cache, matchRunKey } = fixture({
    GET: async (path) =>
      path.includes('/jobs/')
        ? ++lists === 1
          ? late.promise
          : ok({ items: [{ id: 'new' }] })
        : ok({ id: path.includes('old') ? 'old' : 'new' }),
  });
  const key = matchRunKey('alice', 'job');
  const older = cache
    .fetchQuery({ queryKey: key, queryFn: () => store.fetchRun('job') })
    .catch(() => {});
  await cache.refetchQueries({ queryKey: key });
  late.resolve(ok({ items: [{ id: 'old' }] }));
  await older;
  assert.equal(cache.getQueryData(key).id, 'new');
});
