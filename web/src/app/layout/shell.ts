import { ChangeDetectionStrategy, Component, computed, inject } from '@angular/core';
import { RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { NgIcon, provideIcons } from '@ng-icons/core';
import {
  lucideBuilding2,
  lucideFolderOpen,
  lucideLogOut,
  lucideMail,
  lucideSparkles,
  lucideSquareKanban,
  lucideUser,
} from '@ng-icons/lucide';

import { ZardSidebarImports } from '@/shared/components/sidebar';

import { AuthService } from '../core/auth.service';
import { AccountService } from '../core/account.service';
import { EventsService } from '../core/events.service';
import { ConfigService } from '../core/config';

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
      lucideMail,
    }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <z-sidebar-provider class="h-svh min-h-0">
      <z-sidebar zCollapsible="icon">
        <z-sidebar-header>
          <ul z-sidebar-menu>
            <li z-sidebar-menu-item>
              <a
                z-sidebar-menu-button
                zSize="lg"
                routerLink="/jobs"
              >
                <img src="icon.svg" alt="" class="size-8 shrink-0 rounded-md" />
                <span class="text-base font-semibold tracking-tight">zapply</span>
              </a>
            </li>
          </ul>
        </z-sidebar-header>

        <z-sidebar-content>
          @for (group of groups(); track group.label) {
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
              <button
                type="button"
                z-sidebar-menu-button
                (click)="auth.signOut()"
                [zTooltip]="'Sign out ' + (auth.session()?.label ?? '')"
              >
                <ng-icon name="lucideLogOut" />
                <span class="truncate">Sign out · {{ auth.session()?.label }}</span>
              </button>
            </li>
          </ul>
          @if (buildLabel()) {
            <p class="px-2 text-xs text-muted-foreground group-data-[collapsible=icon]:hidden">{{ buildLabel() }}</p>
          }
        </z-sidebar-footer>

        <button z-sidebar-rail></button>
      </z-sidebar>

      <main z-sidebar-inset class="min-h-0 overflow-hidden">
        <header class="flex h-11 shrink-0 items-center gap-2 border-b px-2">
          <button z-sidebar-trigger></button>
          <span
            class="ml-auto flex items-center gap-2 pr-2 text-xs text-muted-foreground"
            [title]="'Live updates: ' + events.status()"
          >
            <span
              class="size-2 rounded-full"
              [class.bg-emerald-500]="events.status() === 'live'"
              [class.bg-amber-500]="events.status() === 'connecting'"
              [class.bg-muted-foreground]="events.status() === 'offline'"
            ></span>
            {{
              events.status() === 'live'
                ? 'Live'
                : events.status() === 'connecting'
                  ? 'Connecting…'
                  : 'Offline'
            }}
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
  private readonly account = inject(AccountService);
  protected readonly events = inject(EventsService);
  private readonly config = inject(ConfigService);
  protected readonly buildLabel = computed(() => {
    const { build_commit, built_at } = this.config.config();
    const parts: string[] = [];
    if (build_commit) parts.push(build_commit.slice(-6));
    if (built_at) {
      const date = new Date(built_at);
      if (!Number.isNaN(date.getTime())) {
        const day = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric' }).format(date);
        const time = new Intl.DateTimeFormat('en-GB', { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).format(date);
        parts.push(`${day}, ${time}`);
      }
    }
    return parts.join(' · ');
  });

  protected readonly groups = computed<{ label: string; items: NavItem[] }[]>(() => [
    {
      label: 'Applications',
      items: [
        { path: '/jobs', label: 'Jobs', icon: 'lucideSquareKanban' },
        { path: '/mail', label: 'Mail', icon: 'lucideMail' },
      ],
    },
    {
      label: 'Library',
      items: [
        { path: '/library/profile', label: 'Profile', icon: 'lucideUser' },
        { path: '/library/experience', label: 'Experience', icon: 'lucideFolderOpen' },
        { path: '/library/organizations', label: 'Organizations', icon: 'lucideBuilding2' },
        { path: '/library/skills', label: 'Skills', icon: 'lucideSparkles' },
      ],
    },
    { label: 'Account', items: [{ path: '/usage', label: 'Usage', icon: 'lucideSparkles' }] },
    ...(this.account.me()?.platform_role === 'owner'
      ? [
          {
            label: 'Platform',
            items: [{ path: '/admin/plans', label: 'Administration', icon: 'lucideBuilding2' }],
          },
        ]
      : []),
  ]);
}
