import { Injectable, signal } from '@angular/core';

/** Runtime settings served by the API at /config.json. */
export interface AppConfig {
  supabase_url: string;
  supabase_anon_key: string;
  dev_login: boolean;
  /** Deployment origin without a trailing slash, e.g. https://zapply.example.com. */
  public_url?: string;
  build_commit?: string;
  built_at?: string;
}

@Injectable({ providedIn: 'root' })
export class ConfigService {
  readonly config = signal<AppConfig>({ supabase_url: '', supabase_anon_key: '', dev_login: false });

  async load(): Promise<void> {
    try {
      const res = await fetch('/config.json', { cache: 'no-cache' });
      if (res.ok) this.config.set(await res.json());
    } catch {
      // Offline or API down: the login page explains what's missing.
    }
  }
}
