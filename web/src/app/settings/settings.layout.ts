import { ChangeDetectionStrategy, Component } from '@angular/core';
import { RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';

/** Settings frame: a heading, a tab per section, and the routed section below. */
@Component({
  selector: 'app-settings-layout',
  imports: [RouterLink, RouterLinkActive, RouterOutlet],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mx-auto max-w-[64rem] px-6 pt-7">
      <h1 class="text-xl font-semibold tracking-tight">Settings</h1>
      <p class="mt-1 text-sm text-muted-foreground">
        Your account and the services zapply works with on your behalf.
      </p>
      <nav class="mt-5 flex gap-1 border-b" aria-label="Settings sections">
        @for (t of tabs; track t.path) {
          <a
            [routerLink]="t.path"
            routerLinkActive="!border-foreground !text-foreground"
            class="-mb-px border-b-2 border-transparent px-3 pb-2.5 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground"
            >{{ t.label }}</a
          >
        }
      </nav>
    </div>
    <router-outlet />
  `,
})
export class SettingsLayout {
  protected readonly tabs = [
    { path: '/settings/account', label: 'Account' },
    { path: '/settings/connections', label: 'Connections' },
  ];
}
