// Exercise production code with deferred responses; the Angular build checks templates.
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const ts = require('typescript');
const test = require('node:test');
const assert = require('node:assert/strict');
const { Subject } = require('rxjs');
const signal = (initial) => {
  let value = initial;
  const read = () => value;
  read.set = (v) => (value = v);
  read.update = (fn) => (value = fn(value));
  return read;
};
class Api {
  static ok(r) {
    if (!r.response.ok) throw Error('request failed');
    return r.data;
  }
}
class AuthService {}
class EventsService {}
const DestroyRef = Symbol('DestroyRef');
const deferred = () => {
  let resolve;
  const promise = new Promise((r) => (resolve = r));
  return { promise, resolve };
};
const ok = (data) => ({ data, response: { ok: true } });
function fixture(file) {
  const effects = [],
    requests = [],
    cleanup = [];
  const auth = { identity: signal('alice') };
  const events = { changes: new Subject(), resync: new Subject() };
  const api = {
    client: {
      PUT(url, options) {
        const d = deferred();
        requests.push({ url, options, ...d });
        return d.promise;
      },
      GET(url, options) {
        const d = deferred();
        requests.push({ url, options, ...d });
        return d.promise;
      },
    },
  };
  const core = {
    signal,
    Injectable: () => (x) => x,
    effect: (fn) => {
      effects.push(fn);
      return { destroy() {} };
    },
    DestroyRef,
    inject(token) {
      if (token === Api) return api;
      if (token === AuthService) return auth;
      if (token === EventsService) return events;
      if (token === DestroyRef) return { onDestroy: (fn) => cleanup.push(fn) };
      return {};
    },
  };
  let source = fs.readFileSync(path.resolve(__dirname, '../src/app', file), 'utf8');
  if (source.includes('@Component(')) {
    const start = source.indexOf('@Component(');
    source = source.slice(0, start) + source.slice(source.indexOf('export class', start));
  }
  const code = ts.transpileModule(source, {
    compilerOptions: {
      module: ts.ModuleKind.CommonJS,
      target: ts.ScriptTarget.ES2022,
      experimentalDecorators: true,
    },
  }).outputText;
  const module = { exports: {} };
  const stub = new Proxy({}, { get: () => () => {} });
  const requireStub = (name) =>
    name === '@angular/core'
      ? core
      : name.endsWith('/api')
        ? { Api }
        : name.endsWith('/auth.service')
          ? { AuthService }
          : name.endsWith('/events.service')
            ? { EventsService }
            : stub;
  vm.runInNewContext(code, {
    module,
    exports: module.exports,
    require: requireStub,
    Promise,
    window: { addEventListener() {}, removeEventListener() {} },
    document: { hidden: false, addEventListener() {}, removeEventListener() {} },
    setTimeout: () => 1,
    clearTimeout() {},
    Date,
    Math,
    Error,
  });
  return {
    types: module.exports,
    effects,
    requests,
    auth,
    events,
    close: () => cleanup.forEach((fn) => fn()),
  };
}

test('owner permissions cannot return from an earlier signed-in identity', async (t) => {
  const f = fixture('core/account.service.ts');
  t.after(f.close);
  const account = new f.types.AccountService();
  const old = account.refresh();
  f.auth.identity.set('bob');
  f.effects[0]();
  assert.equal(account.me(), null);
  f.requests[1].resolve(ok({ id: 'bob', platform_role: 'member', membership: null }));
  await Promise.resolve();
  await Promise.resolve();
  f.requests[0].resolve(
    ok({ id: 'alice', platform_role: 'owner', membership: { plan_name: 'Private' } }),
  );
  await old;
  assert.equal(account.me().id, 'bob');
  assert.equal(account.me().platform_role, 'member');
  assert.equal(account.me().membership, null);
});

test('usage page discards old-account usage and charge responses', async (t) => {
  const f = fixture('usage/usage.page.ts');
  t.after(f.close);
  const page = new f.types.UsagePage();
  const old = page.load();
  f.auth.identity.set('bob');
  const fresh = page.load();
  f.requests[2].resolve(
    ok({ membership: null, next_capacity_at: null, blocked_reason: 'membership_required' }),
  );
  f.requests[3].resolve(ok([]));
  await fresh;
  f.requests[0].resolve(ok({ membership: { plan_name: 'Alice plan' }, next_capacity_at: null }));
  f.requests[1].resolve(ok([{ id: 'private-alice-charge' }]));
  await old;
  assert.equal(page.usage().membership, null);
  assert.equal(page.entries().length, 0);
});

test('a route check receives its owner result while another account refresh is pending', async (t) => {
  const f = fixture('core/account.service.ts');
  t.after(f.close);
  const account = new f.types.AccountService();
  const routeCheck = account.refresh();
  f.effects[0]();
  f.requests[0].resolve(ok({ id: 'alice', platform_role: 'owner', membership: null }));
  const result = await routeCheck;
  assert.equal(result.platform_role, 'owner');
  f.requests[1].resolve(ok({ id: 'alice', platform_role: 'owner', membership: null }));
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(account.me().platform_role, 'owner');
});

test('a late charge page cannot repopulate a refreshed usage list', async (t) => {
  const f = fixture('usage/usage.page.ts');
  t.after(f.close);
  const page = new f.types.UsagePage();
  page.entries.set([{ id: '50' }]);
  const older = page.more();
  const fresh = page.load();
  f.requests[1].resolve(ok({ membership: null, next_capacity_at: null }));
  f.requests[2].resolve(ok([{ id: 'new-charge' }]));
  await fresh;
  f.requests[0].resolve(ok([{ id: 'old-charge' }]));
  await older;
  assert.equal(page.entries().length, 1);
  assert.equal(page.entries()[0].id, 'new-charge');
});

test('summary provider dropdown discards stale model responses and selects the sole endpoint', async () => {
  const f = fixture('admin/model-settings.ts');
  const settings = new f.types.ModelSettings();
  settings.selectSummaryModel('first/model');
  settings.selectSummaryModel('second/model');
  f.requests[1].resolve(ok([{tag:'second/fp8',name:'Second provider'}]));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(settings.summaryEndpoint,'second/fp8');
  f.requests[0].resolve(ok([{tag:'first/fp8',name:'First provider'}]));
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(settings.summaryEndpoint,'second/fp8');
  assert.equal(settings.endpoints()[0].tag,'second/fp8');
});

test('summary provider dropdown preserves saved choices and clears unavailable ones', async () => {
  const f = fixture('admin/model-settings.ts');
  const settings = new f.types.ModelSettings();
  settings.summaryModel='test/model'; settings.summaryEndpoint='saved/fp8';
  const loading=settings.loadEndpoints();
  f.requests[0].resolve(ok([{tag:'saved/fp8',name:'Saved'},{tag:'other/fp8',name:'Other'}]));
  await loading;
  assert.equal(settings.summaryEndpoint,'saved/fp8');
  const refresh=settings.loadEndpoints();
  f.requests[1].resolve(ok([])); await refresh;
  assert.equal(settings.summaryEndpoint,'');
  assert.equal(settings.endpointLoading(),false);
});


test('summary caps load the saved values and save the owner selections', async (t) => {
  const f = fixture('admin/model-settings.ts');
  t.after(f.close);
  const settings = new f.types.ModelSettings();
  assert.equal(settings.summaryMaxOutputTokens, 2048);
  assert.equal(settings.summaryMaxCharacters, 500);
  const loading = settings.load();
  f.requests[0].resolve(ok({version: 1, decision_model: null, summary_model: null, summary_endpoint: null, summary_max_output_tokens: 4096, summary_max_characters: 300}));
  await new Promise(resolve => setImmediate(resolve));
  f.requests[1].resolve(ok([]));
  await new Promise(resolve => setImmediate(resolve));
  f.requests[2].resolve(ok([]));
  await loading;
  assert.equal(settings.summaryMaxOutputTokens, 4096);
  assert.equal(settings.summaryMaxCharacters, 300);
  settings.summaryMaxOutputTokens = 8192;
  settings.summaryMaxCharacters = 100;
  const saving = settings.save();
  assert.equal(f.requests[3].options.body.summary_max_output_tokens, 8192);
  assert.equal(f.requests[3].options.body.summary_max_characters, 100);
  assert.equal(f.requests[3].options.params.header['If-Match'], '"1"');
  f.requests[3].resolve(ok({version: 2, summary_max_output_tokens: 8192, summary_max_characters: 100}));
  await saving;
  assert.equal(settings.settings().summary_max_output_tokens, 8192);
  assert.equal(settings.settings().summary_max_characters, 100);
  assert.equal(settings.busy(), false);
});

test('thinking effort loads from the saved model and saves only efforts that model accepts', async (t) => {
  const f = fixture('admin/model-settings.ts');
  t.after(f.close);
  const flush = () => new Promise((resolve) => setImmediate(resolve));
  const settings = new f.types.ModelSettings();
  const loading = settings.load();
  f.requests[0].resolve(ok({version: 3, decision_model: null, summary_model: 'reason/model', summary_endpoint: 'reason/fp8', summary_max_output_tokens: 2048, summary_max_characters: 500, summary_reasoning_effort: 'low'}));
  await flush();
  f.requests[1].resolve(ok([{tag: 'reason/fp8', name: 'Reason', service_tier: 'default'}]));
  await flush();
  f.requests[2].resolve(ok([
    {id: 'reason/model', name: 'Reason', context_length: 1000, pricing: {prompt: '0.00000002', completion: '0.0000001'}, compatibility_reason: '', reasoning: {mandatory: false, efforts: ['high', 'low', 'off']}},
    {id: 'plain/model', name: 'Plain', context_length: 1000, pricing: {}, compatibility_reason: ''},
  ]));
  await flush();
  f.requests[3].resolve(ok([]));
  await loading;
  assert.equal(settings.summaryEffort, 'low');
  assert.equal(settings.effortUnavailable(), false);
  assert.equal(JSON.stringify(settings.effortOptions().map((o) => o.value)), JSON.stringify(['', 'high', 'low', 'off']));
  assert.equal(JSON.stringify(settings.textModels().map((m) => m.name)), JSON.stringify(['Plain', 'Reason']));
  assert.equal(settings.perMillion('0.00000002'), '$0.02');
  assert.match(settings.effortHint(), /Thinking counts toward the 2,048-token output cap\. Low can use up to about 20%/);

  settings.summaryEffort = 'off';
  const saving = settings.save();
  assert.equal(f.requests[4].options.body.summary_reasoning_effort, 'off');
  assert.equal(f.requests[4].options.params.header['If-Match'], '"3"');
  f.requests[4].resolve(ok({version: 4, decision_model: null, summary_model: 'reason/model', summary_endpoint: 'reason/fp8', summary_max_output_tokens: 2048, summary_max_characters: 500, summary_reasoning_effort: 'off'}));
  await saving;
  assert.equal(settings.summaryEffort, 'off');
  assert.equal(settings.settings().summary_reasoning_effort, 'off');

  settings.summaryEffort = 'xhigh';
  assert.equal(settings.effortUnavailable(), true);
  const requests = f.requests.length;
  await settings.save();
  assert.equal(f.requests.length, requests, 'an unsupported effort must not be sent');

  settings.summaryEffort = '';
  settings.selectSummaryModel('plain/model');
  assert.equal(settings.summaryEffort, '');
  assert.equal(JSON.stringify(settings.effortOptions().map((o) => o.value)), JSON.stringify(['']));
  assert.equal(settings.effortHint(), 'This model does not take a thinking setting.');
});
