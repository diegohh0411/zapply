import { DatePipe } from '@angular/common';
import { ChangeDetectionStrategy, Component, inject } from '@angular/core';
import { RouterLink } from '@angular/router';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideBell, lucideLogOut, lucidePlug, lucideSparkles } from '@ng-icons/lucide';

import { ZardButtonComponent } from '@/shared/components/button';
import { AccountService } from '../core/account.service';
import { AuthService } from '../core/auth.service';
import { NotificationsService } from '../core/notifications.service';

/** Settings › Account: who is signed in, and shortcuts to what they manage. */
@Component({
  selector: 'app-account-settings',
  imports: [DatePipe, NgIcon, RouterLink, ZardButtonComponent],
  providers: [provideIcons({ lucideBell, lucideLogOut, lucidePlug, lucideSparkles })],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="mx-auto max-w-[64rem] space-y-8 px-6 pt-7 pb-12">
      <section aria-labelledby="profile-title">
        <h2 id="profile-title" class="mb-3 text-sm font-semibold">Signed in</h2>
        <div class="flex flex-wrap items-center justify-between gap-4 rounded-xl border p-5">
          <div>
            <p class="font-medium">{{ auth.session()?.label }}</p>
            @if (account.me(); as me) {
              <p class="mt-1 text-sm text-muted-foreground">
                Member since {{ me.created_at | date: 'MMMM d, y' }}
                @if (me.platform_role === 'owner') {
                  · Platform owner
                }
              </p>
            }
          </div>
          <button z-button zType="outline" (click)="auth.signOut()">
            <ng-icon name="lucideLogOut" /> Sign out
          </button>
        </div>
      </section>

      <section aria-labelledby="manage-title">
        <h2 id="manage-title" class="mb-3 text-sm font-semibold">Manage</h2>
        <ul class="divide-y rounded-xl border">
          @for (l of links(); track l.path) {
            <li>
              <a
                [routerLink]="l.path"
                class="flex items-center gap-3 px-5 py-4 transition-colors hover:bg-muted/50 focus-visible:bg-muted/50 focus-visible:outline-none"
              >
                <ng-icon [name]="l.icon" class="text-muted-foreground" />
                <span class="flex-1">
                  <span class="block text-sm font-medium">{{ l.label }}</span>
                  <span class="block text-sm text-muted-foreground">{{ l.hint }}</span>
                </span>
                <span aria-hidden="true" class="text-muted-foreground">›</span>
              </a>
            </li>
          }
        </ul>
      </section>
    </div>
  `,
})
export class AccountSettingsPage {
  protected readonly auth = inject(AuthService);
  protected readonly account = inject(AccountService);
  private readonly notes = inject(NotificationsService);
  protected readonly links = () => [
    {
      path: '/settings/connections',
      icon: 'lucidePlug',
      label: 'Connections',
      hint: 'Connect Gmail so application mail turns into notifications.',
    },
    {
      path: '/notifications',
      icon: 'lucideBell',
      label: 'Notifications',
      hint: this.notes.unread() ? `${this.notes.unread()} unread` : 'You are all caught up.',
    },
    {
      path: '/usage',
      icon: 'lucideSparkles',
      label: 'Usage',
      hint: 'What zapply has done for you and what it cost.',
    },
  ];
}
