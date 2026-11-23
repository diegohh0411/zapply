import { Component, inject } from '@angular/core';
import { RouterOutlet } from '@angular/router';

import { ZardSonnerComponent } from '@/shared/components/sonner';

import { SeoService } from './core/seo.service';

@Component({
  selector: 'app-root',
  imports: [RouterOutlet, ZardSonnerComponent],
  template: `<router-outlet /><z-sonner position="bottom-right" [richColors]="true" />`,
})
export class App {
  constructor() {
    // Starts tracking navigations so each route sets its own meta tags.
    inject(SeoService);
  }
}
