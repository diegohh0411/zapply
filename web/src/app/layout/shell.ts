import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideBuilding2,
  lucideFolderOpen,
  lucideLogOut,
  lucideMoon,
  lucideSparkles,
  lucideSquareKanban,
  lucideSun,
  lucideUser,
} from '@ng-icons/lucide';

import { NgTemplateOutlet } from '@angular/common';

import { AuthService } from '../core/auth.service';
import { EventsService } from '../core/events.service';
import { ThemeService } from './theme';

interface NavItem {
  path: string;
  label: string;
  icon: string;
}

@Component({
  selector: 'app-shell',
  imports: [RouterOutlet, RouterLink, RouterLinkActive, NgIcon, NgTemplateOutlet],
  providers: [
    provideIcons({
      lucideSquareKanban,
      lucideUser,
      lucideFolderOpen,
      lucideBuilding2,
      lucideSparkles,
      lucideLogOut,
      lucideSun,
      lucideMoon,
    }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="flex h-dvh overflow-hidden">
      <aside class="flex w-14 shrink-0 flex-col border-r bg-sidebar md:w-56">
        <a routerLink="/jobs" class="flex h-14 items-center gap-2 px-3.5 md:px-4">
          <img src="icon.svg" alt="" class="size-7 rounded-md" />
          <span class="hidden text-base font-semibold tracking-tight md:inline">zapply</span>
        </a>

        <nav class="flex flex-1 flex-col gap-0.5 overflow-y-auto px-2 py-2" aria-label="Main">
          <p class="hidden px-2 pb-1 pt-2 text-xs font-medium text-muted-foreground md:block">Applications</p>
          @for (item of applications; track item.path) {
            <ng-container *ngTemplateOutlet="link; context: { $implicit: item }" />
          }
          <p class="hidden px-2 pb-1 pt-4 text-xs font-medium text-muted-foreground md:block">Library</p>
          @for (item of library; track item.path) {
            <ng-container *ngTemplateOutlet="link; context: { $implicit: item }" />
          }
        </nav>

        <div class="flex flex-col gap-1 border-t p-2">
          <div class="flex items-center gap-2 px-2 py-1 text-xs text-muted-foreground" [title]="'Live updates: ' + events.status()">
            <span
              class="size-2 shrink-0 rounded-full"
              [class.bg-emerald-500]="events.status() === 'live'"
              [class.bg-amber-500]="events.status() === 'connecting'"
              [class.bg-muted-foreground]="events.status() === 'offline'"
            ></span>
            <span class="hidden truncate md:inline">{{ auth.session()?.label }}</span>
          </div>
          <div class="flex flex-col gap-1 md:flex-row">
            <button
              type="button"
              class="flex flex-1 items-center justify-center gap-2 rounded-md px-2 py-1.5 text-sm text-muted-foreground hover:bg-sidebar-accent hover:text-sidebar-accent-foreground md:justify-start"
              (click)="theme.toggle()"
              [attr.aria-label]="theme.dark() ? 'Light theme' : 'Dark theme'"
            >
              <ng-icon [name]="theme.dark() ? 'lucideSun' : 'lucideMoon'" class="size-4" />
              <span class="hidden md:inline">{{ theme.dark() ? 'Light' : 'Dark' }}</span>
            </button>
            <button
              type="button"
              class="flex flex-1 items-center justify-center gap-2 rounded-md px-2 py-1.5 text-sm text-muted-foreground hover:bg-sidebar-accent hover:text-sidebar-accent-foreground md:justify-start"
              (click)="auth.signOut()"
              aria-label="Sign out"
            >
              <ng-icon name="lucideLogOut" class="size-4" />
              <span class="hidden md:inline">Sign out</span>
            </button>
          </div>
        </div>
      </aside>

      <main class="min-w-0 flex-1 overflow-auto">
        <router-outlet />
      </main>
    </div>

    <ng-template #link let-item>
      <a
        [routerLink]="item.path"
        routerLinkActive="bg-sidebar-accent text-sidebar-accent-foreground font-medium"
        class="flex items-center justify-center gap-2.5 rounded-md px-2 py-1.5 text-sm text-sidebar-foreground/80 hover:bg-sidebar-accent hover:text-sidebar-accent-foreground md:justify-start"
        [attr.aria-label]="item.label"
      >
        <ng-icon [name]="item.icon" class="size-4 shrink-0" />
        <span class="hidden md:inline">{{ item.label }}</span>
      </a>
    </ng-template>
  `,
})
export class Shell {
  protected readonly auth = inject(AuthService);
  protected readonly events = inject(EventsService);
  protected readonly theme = inject(ThemeService);

  protected readonly applications: NavItem[] = [{ path: '/jobs', label: 'Jobs', icon: 'lucideSquareKanban' }];
  protected readonly library: NavItem[] = [
    { path: '/library/profile', label: 'Profile', icon: 'lucideUser' },
    { path: '/library/experience', label: 'Experience', icon: 'lucideFolderOpen' },
    { path: '/library/organizations', label: 'Organizations', icon: 'lucideBuilding2' },
    { path: '/library/skills', label: 'Skills', icon: 'lucideSparkles' },
  ];
}
