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

import { ZardSidebarImports } from '@/shared/components/sidebar';

import { AuthService } from '../core/auth.service';
import { EventsService } from '../core/events.service';
import { ThemeService } from './theme';

interface NavItem {
  path: string;
  label: string;
  icon: string;
}

/** App frame: ZardUI sidebar (collapses to icons; a drawer on mobile) and the routed page. */
@Component({
  selector: 'app-shell',
  imports: [RouterOutlet, RouterLink, RouterLinkActive, NgIcon, ...ZardSidebarImports],
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
    <z-sidebar-provider class="h-svh min-h-0">
      <z-sidebar zCollapsible="icon" #sidebar="zSidebar">
        <z-sidebar-header>
          <ul z-sidebar-menu>
            <li z-sidebar-menu-item>
              <a z-sidebar-menu-button zSize="lg" routerLink="/jobs">
                <img src="icon.svg" alt="" class="size-8 shrink-0 rounded-md" />
                <span class="text-base font-semibold tracking-tight">zapply</span>
              </a>
            </li>
          </ul>
        </z-sidebar-header>

        <z-sidebar-content>
          @for (group of groups; track group.label) {
            <z-sidebar-group>
              <z-sidebar-group-label>{{ group.label }}</z-sidebar-group-label>
              <z-sidebar-group-content>
                <ul z-sidebar-menu>
                  @for (item of group.items; track item.path) {
                    <li z-sidebar-menu-item>
                      <a
                        z-sidebar-menu-button
                        [routerLink]="item.path"
                        routerLinkActive
                        #rla="routerLinkActive"
                        [zActive]="rla.isActive"
                        [zTooltip]="item.label"
                        (click)="sidebar.sidebarService.setOpenMobile(false)"
                      >
                        <ng-icon [name]="item.icon" />
                        <span>{{ item.label }}</span>
                      </a>
                    </li>
                  }
                </ul>
              </z-sidebar-group-content>
            </z-sidebar-group>
          }
        </z-sidebar-content>

        <z-sidebar-footer>
          <ul z-sidebar-menu>
            <li z-sidebar-menu-item>
              <button type="button" z-sidebar-menu-button (click)="theme.toggle()" [zTooltip]="theme.dark() ? 'Light theme' : 'Dark theme'">
                <ng-icon [name]="theme.dark() ? 'lucideSun' : 'lucideMoon'" />
                <span>{{ theme.dark() ? 'Light theme' : 'Dark theme' }}</span>
              </button>
            </li>
            <li z-sidebar-menu-item>
              <button type="button" z-sidebar-menu-button (click)="auth.signOut()" [zTooltip]="'Sign out ' + (auth.session()?.label ?? '')">
                <ng-icon name="lucideLogOut" />
                <span class="truncate">Sign out · {{ auth.session()?.label }}</span>
              </button>
            </li>
          </ul>
        </z-sidebar-footer>

        <button z-sidebar-rail></button>
      </z-sidebar>

      <main z-sidebar-inset class="min-h-0 overflow-hidden">
        <header class="flex h-11 shrink-0 items-center gap-2 border-b px-2">
          <button z-sidebar-trigger></button>
          <span class="ml-auto flex items-center gap-2 pr-2 text-xs text-muted-foreground" [title]="'Live updates: ' + events.status()">
            <span
              class="size-2 rounded-full"
              [class.bg-emerald-500]="events.status() === 'live'"
              [class.bg-amber-500]="events.status() === 'connecting'"
              [class.bg-muted-foreground]="events.status() === 'offline'"
            ></span>
            {{ events.status() === 'live' ? 'Live' : events.status() === 'connecting' ? 'Connecting…' : 'Offline' }}
          </span>
        </header>
        <div class="min-h-0 flex-1 overflow-auto">
          <router-outlet />
        </div>
      </main>
    </z-sidebar-provider>
  `,
})
export class Shell {
  protected readonly auth = inject(AuthService);
  protected readonly events = inject(EventsService);
  protected readonly theme = inject(ThemeService);

  protected readonly groups: { label: string; items: NavItem[] }[] = [
    { label: 'Applications', items: [{ path: '/jobs', label: 'Jobs', icon: 'lucideSquareKanban' }] },
    {
      label: 'Library',
      items: [
        { path: '/library/profile', label: 'Profile', icon: 'lucideUser' },
        { path: '/library/experience', label: 'Experience', icon: 'lucideFolderOpen' },
        { path: '/library/organizations', label: 'Organizations', icon: 'lucideBuilding2' },
        { path: '/library/skills', label: 'Skills', icon: 'lucideSparkles' },
      ],
    },
  ];
}
