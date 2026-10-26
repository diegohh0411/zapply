import { Component } from '@angular/core';
import { RouterOutlet } from '@angular/router';

import { ZardSonnerComponent } from '@/shared/components/sonner';

@Component({
  selector: 'app-root',
  imports: [RouterOutlet, ZardSonnerComponent],
  template: `<router-outlet /><z-sonner position="bottom-right" [richColors]="true" />`,
})
export class App {}
