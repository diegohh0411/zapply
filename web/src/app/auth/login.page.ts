import { ChangeDetectionStrategy, Component, inject, signal } from '@angular/core';
import { FormsModule } from '@angular/forms';
import { Router } from '@angular/router';

import { ZardButtonComponent } from '@/shared/components/button';
import { ZardInputComponent } from '@/shared/components/input';
import { ZardSeparatorComponent } from '@/shared/components/separator';

import { AuthService } from '../core/auth.service';

@Component({
  selector: 'app-login',
  imports: [FormsModule, ZardButtonComponent, ZardInputComponent, ZardSeparatorComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="flex min-h-dvh items-center justify-center bg-muted/40 p-4">
      <div class="w-full max-w-sm rounded-xl border bg-card p-6 shadow-sm">
        <div class="mb-6 flex flex-col items-center gap-3 text-center">
          <img src="icon.svg" alt="" class="size-12 rounded-xl" />
          <div>
            <h1 class="text-xl font-semibold tracking-tight">Sign in to zapply</h1>
            <p class="text-sm text-muted-foreground">Tailor your CV to each job and track every application.</p>
          </div>
        </div>

        @if (auth.supabaseEnabled()) {
          @if (sent()) {
            <p class="rounded-md bg-muted p-3 text-sm" role="status">
              Check <strong>{{ email }}</strong> for a sign-in link.
            </p>
          } @else {
            <form class="flex flex-col gap-3" (ngSubmit)="magicLink()">
              <label class="text-sm font-medium" for="email">Email</label>
              <input z-input id="email" name="email" type="email" autocomplete="email" required [(ngModel)]="email" placeholder="you@example.com" />
              <button z-button type="submit" [zLoading]="busy()" [zDisabled]="!email">Email me a sign-in link</button>
            </form>
            <div class="my-4 flex items-center gap-3 text-xs text-muted-foreground">
              <z-separator class="flex-1" /> or <z-separator class="flex-1" />
            </div>
            <button z-button zType="outline" class="w-full" type="button" (click)="google()">Continue with Google</button>
          }
        }

        @if (auth.devEnabled()) {
          @if (auth.supabaseEnabled()) {
            <z-separator class="my-5" />
          }
          <form class="flex flex-col gap-3" (ngSubmit)="dev()">
            <label class="text-sm font-medium" for="sub">Development sign-in</label>
            <input z-input id="sub" name="sub" [(ngModel)]="sub" placeholder="alice" />
            <button z-button zType="secondary" type="submit" [zLoading]="busy()" [zDisabled]="!sub">Sign in as {{ sub || '…' }}</button>
            <p class="text-xs text-muted-foreground">Mints a token with AUTH_DEV_SECRET. Only available in development.</p>
          </form>
        }

        @if (!auth.supabaseEnabled() && !auth.devEnabled()) {
          <p class="text-sm text-muted-foreground" role="alert">
            Sign-in isn't configured. Set <code>SUPABASE_URL</code> and <code>SUPABASE_ANON_KEY</code> on the API,
            or <code>AUTH_DEV_SECRET</code> in development.
          </p>
        }

        @if (error()) {
          <p class="mt-4 text-sm text-destructive" role="alert">{{ error() }}</p>
        }
      </div>
    </div>
  `,
})
export class LoginPage {
  protected readonly auth = inject(AuthService);
  private readonly router = inject(Router);

  protected email = '';
  protected sub = 'alice';
  protected readonly busy = signal(false);
  protected readonly sent = signal(false);
  protected readonly error = signal<string | null>(null);

  protected async magicLink(): Promise<void> {
    await this.run(async () => {
      await this.auth.sendMagicLink(this.email.trim());
      this.sent.set(true);
    });
  }

  protected async google(): Promise<void> {
    await this.run(() => this.auth.signInWithGoogle());
  }

  protected async dev(): Promise<void> {
    await this.run(async () => {
      await this.auth.devSignIn(this.sub.trim());
      await this.router.navigateByUrl('/jobs');
    });
  }

  private async run(fn: () => Promise<void>): Promise<void> {
    this.busy.set(true);
    this.error.set(null);
    try {
      await fn();
    } catch (e) {
      this.error.set((e as Error).message);
    } finally {
      this.busy.set(false);
    }
  }
}
