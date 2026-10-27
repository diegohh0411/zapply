// Production MatchStore methods, controlled deferred HTTP and signal state.
const fs=require('node:fs'); const vm=require('node:vm'); const assert=require('node:assert/strict'); const test=require('node:test');
const ts=require('typescript'); const {Subject}=require('rxjs');
function signal(value){const s=()=>value;s.set=v=>value=v;s.update=f=>value=f(value);return s;}
class AuthService{} class EventsService{} class ApiError extends Error{} class Api{static ok(r){if(!r.response.ok)throw new Error('request failed');return r.data;}}
const ok=data=>({data,response:{ok:true}});const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return {promise,resolve};};
function fixture(client){
 const auth={identity:signal('alice')};const events={changes:new Subject(),resync:new Subject()};
 const core={signal,effect:()=>{},untracked:f=>f(),Injectable:()=>x=>x,DestroyRef:{},inject:token=>token===AuthService?auth:token===EventsService?events:{client}};
 const source=fs.readFileSync(require('node:path').resolve(__dirname,'../src/app/jobs/match/match.store.ts'),'utf8');
 const code=ts.transpileModule(source,{compilerOptions:{module:ts.ModuleKind.CommonJS,target:ts.ScriptTarget.ES2022,experimentalDecorators:true}}).outputText;
 const module={exports:{}};vm.runInNewContext(code,{module,exports:module.exports,Map,Set,crypto:require('node:crypto').webcrypto,require:name=>name==='@angular/core'?core:name==='@angular/core/rxjs-interop'?{takeUntilDestroyed:()=>s=>s}:name.endsWith('/auth.service')?{AuthService}:name.endsWith('/events.service')?{EventsService}:{Api,ApiError}});
 return {store:new module.exports.MatchStore(),auth,events};
}
test('identity change discards late matching responses and clears private state',async()=>{
 const waiting=deferred();const {store,auth}=fixture({GET:()=>waiting.promise});const request=store.load('job');auth.identity.set('bob');store.sync();waiting.resolve(ok({items:[{id:'alice-run'}]}));await request;assert.equal(store.runs().size,0);assert.equal(store.estimates().size,0);
});
test('newer requests win when an earlier load finishes late',async()=>{
 const waiting=deferred();let lists=0;
 const {store}=fixture({GET:async(path)=>path.includes('/jobs/')?(++lists===1?waiting.promise:ok({items:[{id:'new'}]})):ok({id:'new',version:2})});
 const older=store.load('job');await store.load('job');waiting.resolve(ok({items:[{id:'old'}]}));await older;assert.equal(store.runs().get('job').id,'new');
});
test('correction round trip reloads the completed run',async()=>{
 const calls=[];const {store}=fixture({PUT:async(path,req)=>{calls.push(req.body.class);return ok(null)},GET:async(path)=>path.includes('/jobs/')?ok({items:[{id:'run'}]}):ok({id:'run',version:2})});
 await store.correct('job','hash','ignore');assert.deepEqual(calls,['ignore']);assert.equal(store.runs().get('job').version,2);
});
test('SSE refreshes only watched jobs',async()=>{
 const {store,events}=fixture({GET:async()=>ok({items:[]})});let calls=0;store.load=async()=>{calls++};store.watched.add('job');events.changes.next({topic:'match_run.changed',data:{job_id:'other'}});events.changes.next({topic:'match_run.changed',data:{job_id:'job'}});assert.equal(calls,1);
});
