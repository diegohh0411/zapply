import { inject, Injectable } from '@angular/core';
import { Meta } from '@angular/platform-browser';
import { ActivatedRouteSnapshot, NavigationEnd, Router } from '@angular/router';
import { filter } from 'rxjs';

/** Route data that drives page-level meta tags. The deepest value along the route wins. */
interface SeoData {
  description?: string;
  robots?: string;
}

const DEFAULT_DESCRIPTION = 'Tailor your CV to each job and track every application with zapply.';
const DEFAULT_ROBOTS = 'index, follow';

/**
 * Keeps the description and robots meta tags in step with the current route.
 * Page titles come from each route's `title`, which the router applies itself.
 */
@Injectable({ providedIn: 'root' })
export class SeoService {
  private readonly router = inject(Router);
  private readonly meta = inject(Meta);

  constructor() {
    this.router.events
      .pipe(filter((event) => event instanceof NavigationEnd))
      .subscribe(() => this.apply());
  }

  private apply(): void {
    const data = this.routeData();
    this.meta.updateTag({ name: 'description', content: data.description ?? DEFAULT_DESCRIPTION });
    this.meta.updateTag({ name: 'robots', content: data.robots ?? DEFAULT_ROBOTS });
  }

  private routeData(): SeoData {
    let route = this.router.routerState.snapshot.root;
    while (route.firstChild) route = route.firstChild;
    return route.pathFromRoot.reduce<SeoData>(
      (merged, step: ActivatedRouteSnapshot) => ({ ...merged, ...step.data }),
      {},
    );
  }
}
