// Exercise production sidebar state and gestures without a browser or backend.
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');
const assert = require('node:assert/strict');
const ts = require('typescript');
const { Subject } = require('rxjs');
class BreakpointObserver {}
class Router {}
class NavigationEnd {}
class NavigationSkipped {
  constructor(code) {
    this.code = code;
  }
}
const NavigationSkippedCode = { IgnoredSameUrlNavigation: 0, IgnoredByUrlHandlingStrategy: 1 };
const DOCUMENT = Symbol('DOCUMENT');
function load(file, core, extra = {}) {
  const source = fs.readFileSync(path.join(__dirname, '../src/app/shared/components/sidebar', file), 'utf8');
  const module = { exports: {} };
  const stripDecorators = (context) => (root) => {
    const visit = (node) => {
      if (ts.isDecorator(node)) return undefined;
      return ts.visitEachChild(node, visit, context);
    };
    return ts.visitNode(root, visit);
  };
  const code = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
    transformers: { before: [stripDecorators] },
  }).outputText;
  vm.runInNewContext(code, {
    module,
    exports: module.exports,
    require(name) {
      if (name === '@angular/core') return core;
      if (name === '@angular/common') return { DOCUMENT, isPlatformBrowser: () => true };
      if (name === '@angular/cdk/layout') return { BreakpointObserver };
      if (name === '@angular/router') return { Router, NavigationEnd, NavigationSkipped, NavigationSkippedCode };
      if (name === '@angular/core/rxjs-interop') return extra.interop;
      if (name === 'rxjs') return require(name);
      if (name.endsWith('sidebar.constants')) return { ZARD_SIDEBAR_MOBILE_BREAKPOINT: '(max-width: 767px)' };
      return extra[name] ?? {};
    },
  });
  return module.exports;
}
async function fixture(t) {
  const core = await import('@angular/core');
  const interop = await import('@angular/core/rxjs-interop');
  const pending = new Set();
  const breakpoints = new Subject();
  const events = new Subject();
  const document = {
    cookie: '',
    body: { style: { overflow: 'auto' } },
    documentElement: { style: { overflow: '' } },
  };
  const injector = core.createEnvironmentInjector([
    { provide: core.ɵChangeDetectionScheduler, useValue: { notify() {} } },
    {
      provide: core.ɵEffectScheduler,
      useValue: {
        add: (e) => pending.add(e),
        schedule: (e) => pending.add(e),
        remove: (e) => pending.delete(e),
      },
    },
    { provide: core.PLATFORM_ID, useValue: 'browser' },
    { provide: DOCUMENT, useValue: document },
    { provide: BreakpointObserver, useValue: { observe: () => breakpoints } },
    { provide: Router, useValue: { events } },
  ]);
  const { ZardSidebarService } = load('sidebar.service.ts', core, { interop });
  const service = core.runInInjectionContext(injector, () => new ZardSidebarService());
  const flush = () => {
    while (pending.size) {
      const batch = [...pending];
      pending.clear();
      batch.forEach((e) => e.run());
    }
  };
  const resize = (matches) => {
    breakpoints.next({ matches });
    flush();
  };
  t.after(() => {
    if (!injector.destroyed) injector.destroy();
  });
  resize(true);
  return { service, flush, resize, events, document, injector };
}
test('navigation dismisses only after success or selecting the current URL', async (t) => {
  const f = await fixture(t);
  f.service.setOpenMobile(true);
  f.flush();
  for (const type of ['NavigationStart', 'NavigationCancel', 'NavigationError']) {
    f.events.next({ type });
    assert.equal(f.service.openMobile(), true, type);
  }
  f.events.next(new NavigationSkipped(NavigationSkippedCode.IgnoredByUrlHandlingStrategy));
  assert.equal(f.service.openMobile(), true);
  f.events.next(new NavigationEnd());
  assert.equal(f.service.openMobile(), false);
  f.service.setOpenMobile(true);
  f.events.next(new NavigationSkipped(NavigationSkippedCode.IgnoredSameUrlNavigation));
  assert.equal(f.service.openMobile(), false);
});
test('scroll lock restores styles on dismissal, resize and teardown; desktop state is independent', async (t) => {
  const f = await fixture(t);
  f.service.setOpen(false);
  for (const close of [() => f.service.setOpenMobile(false), () => f.resize(false), () => f.injector.destroy()]) {
    f.resize(true);
    f.service.setOpenMobile(true);
    f.flush();
    assert.equal(f.document.body.style.overflow, 'hidden');
    assert.equal(f.document.documentElement.style.overflow, 'hidden');
    close();
    f.flush();
    assert.equal(f.document.body.style.overflow, 'auto');
    assert.equal(f.document.documentElement.style.overflow, '');
    assert.equal(f.service.open(), false);
  }
});
test('returning to mobile does not reopen a stale drawer', async (t) => {
  const f = await fixture(t);
  f.service.setOpenMobile(true);
  f.flush();
  f.resize(false);
  f.resize(true);
  assert.equal(f.service.openMobile(), false);
});
function component() {
  const { ZardSidebarComponent } = load('sidebar.component.ts', {});
  // Gesture methods need only their own state and service, not Angular template queries.
  const drawer = Object.create(ZardSidebarComponent.prototype);
  let open = true;
  drawer.sidebarService = {
    openMobile: () => open,
    setOpenMobile: (value) => {
      open = value;
    },
  };
  drawer.zSide = () => 'left';
  return { drawer, isOpen: () => open };
}
function touch(x, y, id = 1, count = 1) {
  return {
    changedTouches: [{ clientX: x, clientY: y, identifier: id }],
    touches: Array(count).fill({}),
    cancelable: true,
    prevented: false,
    preventDefault() {
      this.prevented = true;
    },
  };
}
test('only an intentional single-finger swipe toward the drawer edge dismisses', () => {
  for (const [side, x, y, closes] of [
    ['left', 20, 100, true],
    ['right', 180, 100, true],
    ['left', 180, 100, false],
    ['left', 80, 100, false],
    ['left', 20, 200, false],
  ]) {
    const f = component();
    f.drawer.zSide = () => side;
    f.drawer.onDrawerTouchStart(touch(100, 100));
    const end = touch(x, y, 1, 0);
    f.drawer.onDrawerTouchEnd(end);
    assert.equal(f.isOpen(), !closes);
    assert.equal(end.prevented, closes, 'closing swipe suppresses a link click');
  }
});
test('canceled touches, multi-touch and mismatched touch identifiers never dismiss', () => {
  for (const mode of ['cancel', 'multi', 'other']) {
    const f = component();
    f.drawer.onDrawerTouchStart(touch(100, 100));
    if (mode === 'cancel') f.drawer.onDrawerTouchCancel();
    if (mode === 'multi') f.drawer.onDrawerTouchStart(touch(100, 100, 2, 2));
    f.drawer.onDrawerTouchEnd(touch(0, 100, mode === 'other' ? 2 : 1, 0));
    assert.equal(f.isOpen(), true);
  }
});
test('Escape respects nested controls and consumes the event when it dismisses', () => {
  const f = component();
  f.drawer.onEscape({ defaultPrevented: true });
  assert.equal(f.isOpen(), true);
  let prevented = false,
    stopped = false;
  f.drawer.onEscape({
    defaultPrevented: false,
    preventDefault() {
      prevented = true;
    },
    stopPropagation() {
      stopped = true;
    },
  });
  assert.equal(f.isOpen(), false);
  assert.ok(prevented && stopped);
});

test('focus enters on every opening and returns after close, resize or destruction', () => {
  let beforeRender, afterRender, destroy;
  let mobile = true, open = false, drawerPresent = true;
  let captures = 0, restores = 0;
  const trigger = { isConnected: true, focus() { restores++; } };
  const document = { activeElement: trigger };
  const service = { isMobile: () => mobile, openMobile: () => open };
  const DestroyRef = Symbol('DestroyRef');
  let query = 0;
  const core = {
    DestroyRef,
    inject: token => token === DOCUMENT ? document : token === DestroyRef ? { onDestroy: cb => { destroy = cb; } } : service,
    input: value => () => value,
    computed: fn => fn,
    viewChild: () => ++query === 1 ? () => drawerPresent ? { nativeElement: {} } : undefined : () => ({ focusTrap: { focusInitialElement() { captures++; } } }),
    effect: cb => { beforeRender = cb; },
    afterRenderEffect: cb => { afterRender = cb; },
  };
  const { ZardSidebarComponent } = load('sidebar.component.ts', core);
  new ZardSidebarComponent();
  afterRender();
  assert.equal(captures, 0, 'closed drawer never steals focus');
  for (let i = 0; i < 2; i++) {
    open = true;
    beforeRender();
    document.activeElement = {}; // Simulate focus leaving the now-inert page.
    afterRender();
    assert.equal(captures, i + 1);
    open = false;
    afterRender();
    assert.equal(restores, i + 1);
    document.activeElement = trigger;
  }
  open = true;
  beforeRender();
  afterRender();
  mobile = false;
  drawerPresent = false;
  afterRender();
  assert.equal(restores, 3, 'resize restores focus');
  mobile = true;
  drawerPresent = true;
  beforeRender();
  afterRender();
  destroy();
  assert.equal(restores, 4, 'teardown restores focus');
});
