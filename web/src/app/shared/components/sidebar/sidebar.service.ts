import { BreakpointObserver } from '@angular/cdk/layout';
import { DOCUMENT, isPlatformBrowser } from '@angular/common';
import { computed, effect, inject, Injectable, PLATFORM_ID, REQUEST, signal } from '@angular/core';
import { takeUntilDestroyed, toSignal } from '@angular/core/rxjs-interop';

import { NavigationEnd, NavigationSkipped, NavigationSkippedCode, Router } from '@angular/router';

import { map, of } from 'rxjs';

import {
  ZARD_SIDEBAR_COOKIE_MAX_AGE,
  ZARD_SIDEBAR_COOKIE_NAME,
  ZARD_SIDEBAR_MOBILE_BREAKPOINT,
} from '@/shared/components/sidebar/sidebar.constants';

let nextSidebarId = 0;

export type ZardSidebarState = 'expanded' | 'collapsed';

/**
 * Controls a single sidebar. The Angular counterpart of shadcn's `useSidebar()` hook.
 *
 * Provided by `ZardSidebarProviderComponent` — never `providedIn: 'root'` — so two providers on the
 * same page keep independent states, exactly like two `SidebarProvider` in React.
 */
@Injectable()
export class ZardSidebarService {
  readonly mobileId = `z-sidebar-mobile-${nextSidebarId++}`;

  private readonly breakpointObserver = inject(BreakpointObserver);
  private readonly document = inject(DOCUMENT);
  private readonly request = inject(REQUEST, { optional: true });
  private readonly isBrowser = isPlatformBrowser(inject(PLATFORM_ID));

  /** Persisted state, read from `document.cookie` on the client and from the `Cookie` header on the server. */
  private readonly persistedOpen = this.readPersistedOpen();

  private readonly internalOpen = signal(this.persistedOpen ?? true);
  private readonly internalOpenMobile = signal(false);

  /**
   * False once the provider set an explicit `zDefaultOpen`. Such a provider never reads the cookie
   * back, so writing it would only change the initial state of every other provider on the site.
   */
  private persistOpen = true;

  /** Mirrors the provider's `zOpen` input. `undefined` means the provider is uncontrolled. */
  readonly controlledOpen = signal<boolean | undefined>(undefined);

  /** Wired by the provider so `setOpen` can emit its `zOpenChange` output. */
  onOpenChange?: (open: boolean) => void;

  readonly isMobile = toSignal(
    this.isBrowser
      ? this.breakpointObserver.observe(ZARD_SIDEBAR_MOBILE_BREAKPOINT).pipe(map(result => result.matches))
      : of(false),
    { initialValue: false },
  );

  readonly open = computed(() => this.controlledOpen() ?? this.internalOpen());
  readonly openMobile = this.internalOpenMobile.asReadonly();
  readonly state = computed<ZardSidebarState>(() => (this.open() ? 'expanded' : 'collapsed'));

  constructor() {
    effect(() => {
      if (!this.isMobile()) this.internalOpenMobile.set(false);
    });

    effect(onCleanup => {
      if (!this.isBrowser || !this.isMobile() || !this.openMobile()) return;
      const body = this.document.body.style;
      const root = this.document.documentElement.style;
      const bodyOverflow = body.overflow;
      const rootOverflow = root.overflow;
      body.overflow = root.overflow = 'hidden';
      onCleanup(() => {
        body.overflow = bodyOverflow;
        root.overflow = rootOverflow;
      });
    });

    // Wait for routing to finish, including same-URL links. Canceled/failed navigation and
    // modified clicks that open a new tab should leave the drawer open.
    inject(Router, { optional: true })
      ?.events.pipe(takeUntilDestroyed())
      .subscribe(event => {
        if (
          event instanceof NavigationEnd ||
          (event instanceof NavigationSkipped && event.code === NavigationSkippedCode.IgnoredSameUrlNavigation)
        ) {
          this.setOpenMobile(false);
        }
      });
  }

  setOpen(value: boolean | ((open: boolean) => boolean)): void {
    const openState = typeof value === 'function' ? value(this.open()) : value;

    // In controlled mode the consumer owns the state — only report the request.
    if (this.controlledOpen() === undefined) {
      this.internalOpen.set(openState);
    }

    this.onOpenChange?.(openState);
    this.persist(openState);
  }

  setOpenMobile(open: boolean): void {
    this.internalOpenMobile.set(open);
  }

  toggleSidebar(): void {
    if (this.isMobile()) {
      this.internalOpenMobile.update(open => !open);
    } else {
      this.setOpen(open => !open);
    }
  }

  /**
   * Applies the provider's `zDefaultOpen`.
   *
   * shadcn's provider only ever writes the cookie: the app reads it server-side and feeds it back in
   * as `defaultOpen`. Angular has no server component to do that, so the service reads it too — but
   * only as the fallback. An explicit `zDefaultOpen` still wins, which keeps that input meaningful
   * and stops one provider's persisted state from deciding for every other provider on the page.
   * Such a provider also stops writing the cookie: a state it never reads back is not persisted.
   */
  applyDefaultOpen(defaultOpen: boolean | undefined): void {
    if (defaultOpen !== undefined) {
      this.persistOpen = false;
      this.internalOpen.set(defaultOpen);
      return;
    }

    if (this.persistedOpen === undefined) {
      this.internalOpen.set(true);
    }
  }

  private persist(open: boolean): void {
    if (!this.isBrowser || !this.persistOpen) {
      return;
    }

    this.document.cookie = `${ZARD_SIDEBAR_COOKIE_NAME}=${open}; path=/; max-age=${ZARD_SIDEBAR_COOKIE_MAX_AGE}`;
  }

  private readPersistedOpen(): boolean | undefined {
    const cookies = this.isBrowser ? this.document.cookie : this.request?.headers?.get('cookie');
    if (!cookies) {
      return undefined;
    }

    const match = new RegExp(`(?:^|;\\s*)${ZARD_SIDEBAR_COOKIE_NAME}=(true|false)`).exec(cookies);
    return match ? match[1] === 'true' : undefined;
  }
}
