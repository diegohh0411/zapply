import { DOCUMENT } from '@angular/common';
import { inject, Injectable } from '@angular/core';
import { Meta } from '@angular/platform-browser';
import { ActivatedRouteSnapshot, NavigationEnd, Router } from '@angular/router';
import { filter } from 'rxjs';

import { ConfigService } from './config';

/** Route data that drives page-level meta tags. The deepest value along the route wins. */
interface SeoData {
  description?: string;
  robots?: string;
}

const DEFAULT_DESCRIPTION = 'Tailor your CV to each job and track every application with zapply.';
const DEFAULT_ROBOTS = 'index, follow';

/**
 * Keeps the description, robots and canonical tags in step with the current route.
 * Page titles come from each route's `title`, which the router applies itself.
 */
@Injectable({ providedIn: 'root' })
export class SeoService {
  private readonly router = inject(Router);
  private readonly meta = inject(Meta);
  private readonly config = inject(ConfigService);
  private readonly doc = inject(DOCUMENT);

  constructor() {
    this.router.events
      .pipe(filter((event) => event instanceof NavigationEnd))
      .subscribe(() => this.apply());
  }

  private apply(): void {
    const data = this.routeData();
    this.meta.updateTag({ name: 'description', content: data.description ?? DEFAULT_DESCRIPTION });
    this.meta.updateTag({ name: 'robots', content: data.robots ?? DEFAULT_ROBOTS });
    this.setCanonical(this.canonicalUrl());
  }

  private routeData(): SeoData {
    let route = this.router.routerState.snapshot.root;
    while (route.firstChild) route = route.firstChild;
    return route.pathFromRoot.reduce<SeoData>(
      (merged, step: ActivatedRouteSnapshot) => ({ ...merged, ...step.data }),
      {},
    );
  }

  /** The current path on the deployment origin, without query or fragment. Null if PUBLIC_URL is unset. */
  private canonicalUrl(): string | null {
    const origin = this.config.config().public_url;
    if (!origin) return null;
    const path = this.router.url.split(/[?#]/)[0];
    return origin + path;
  }

  private setCanonical(href: string | null): void {
    let link = this.doc.head.querySelector<HTMLLinkElement>('link[rel="canonical"]');
    if (!href) {
      link?.remove();
      this.meta.removeTag("property='og:url'");
      return;
    }
    if (!link) {
      link = this.doc.createElement('link');
      link.rel = 'canonical';
      this.doc.head.appendChild(link);
    }
    link.href = href;
    this.meta.updateTag({ property: 'og:url', content: href });
  }
}
