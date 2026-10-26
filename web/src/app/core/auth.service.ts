import { computed, inject, Injectable, signal } from '@angular/core';
import { Router } from '@angular/router';
import { createClient, type SupabaseClient } from '@supabase/supabase-js';

import { ConfigService } from './config';

const DEV_TOKEN_KEY = 'zapply.devToken';

export interface Session {
  token: string;
  label: string; // email or dev subject
  expiresAt: number; // ms
}

/**
 * Sign-in through Supabase Auth (production) or dev tokens minted by the API
 * (development only). The API verifies either kind of JWT.
 */
@Injectable({ providedIn: 'root' })
export class AuthService {
  private readonly config = inject(ConfigService);
  private readonly router = inject(Router);
  private supabase: SupabaseClient | null = null;

  readonly session = signal<Session | null>(null);
  /** Stable across token refreshes, distinct for each authenticated principal. */
  readonly identity = computed(() => {
    const s = this.session();
    if (!s) return null;
    try {
      const claims = JSON.parse(atob(s.token.split('.')[1].replace(/-/g, '+').replace(/_/g, '/')));
      return JSON.stringify([claims.iss, claims.sub]);
    } catch {
      return s.token;
    }
  });
  readonly signedIn = computed(() => this.session() !== null);
  readonly supabaseEnabled = computed(() => !!this.config.config().supabase_url);
  readonly devEnabled = computed(() => this.config.config().dev_login);

  async init(): Promise<void> {
    const c = this.config.config();
    if (c.supabase_url && c.supabase_anon_key) {
      this.supabase = createClient(c.supabase_url, c.supabase_anon_key, {
        auth: { persistSession: true, autoRefreshToken: true, detectSessionInUrl: true },
      });
      this.supabase.auth.onAuthStateChange((_event, s) => {
        if (s) {
          this.session.set({
            token: s.access_token,
            label: s.user.email ?? s.user.id,
            expiresAt: (s.expires_at ?? 0) * 1000,
          });
        } else if (!this.devSession()) {
          this.session.set(null);
        }
      });
      const { data } = await this.supabase.auth.getSession();
      if (data.session) {
        const s = data.session;
        this.session.set({
          token: s.access_token,
          label: s.user.email ?? s.user.id,
          expiresAt: (s.expires_at ?? 0) * 1000,
        });
        return;
      }
    }
    const dev = this.devSession();
    if (dev) this.session.set(dev);
  }

  /** A valid bearer token, refreshed if it is about to expire. */
  async token(): Promise<string | null> {
    const s = this.session();
    if (!s) return null;
    if (this.supabase && s.expiresAt - Date.now() < 60_000 && !this.devSession()) {
      const { data } = await this.supabase.auth.refreshSession();
      if (data.session) {
        this.session.set({
          token: data.session.access_token,
          label: data.session.user.email ?? data.session.user.id,
          expiresAt: (data.session.expires_at ?? 0) * 1000,
        });
        return data.session.access_token;
      }
    }
    if (s.expiresAt < Date.now()) {
      await this.signOut();
      return null;
    }
    return s.token;
  }

  async sendMagicLink(email: string): Promise<void> {
    if (!this.supabase) throw new Error('Supabase is not configured');
    const { error } = await this.supabase.auth.signInWithOtp({
      email,
      options: { emailRedirectTo: window.location.origin + '/jobs' },
    });
    if (error) throw error;
  }

  async signInWithGoogle(): Promise<void> {
    if (!this.supabase) throw new Error('Supabase is not configured');
    const { error } = await this.supabase.auth.signInWithOAuth({
      provider: 'google',
      options: { redirectTo: window.location.origin + '/jobs' },
    });
    if (error) throw error;
  }

  async devSignIn(sub: string): Promise<void> {
    const res = await fetch('/dev/token', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ sub }),
    });
    if (!res.ok) throw new Error('Dev sign-in failed (' + res.status + ')');
    const { token } = (await res.json()) as { token: string };
    const s: Session = { token, label: sub, expiresAt: jwtExpiry(token) };
    localStorage.setItem(DEV_TOKEN_KEY, JSON.stringify(s));
    this.session.set(s);
  }

  async signOut(): Promise<void> {
    localStorage.removeItem(DEV_TOKEN_KEY);
    if (this.supabase) await this.supabase.auth.signOut();
    this.session.set(null);
    await this.router.navigateByUrl('/login');
  }

  private devSession(): Session | null {
    try {
      const s = JSON.parse(localStorage.getItem(DEV_TOKEN_KEY) ?? 'null') as Session | null;
      return s && s.expiresAt > Date.now() ? s : null;
    } catch {
      return null;
    }
  }
}

function jwtExpiry(token: string): number {
  try {
    const payload = JSON.parse(atob(token.split('.')[1].replace(/-/g, '+').replace(/_/g, '/')));
    return payload.exp * 1000;
  } catch {
    return Date.now() + 3600_000;
  }
}
