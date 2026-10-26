import {
  type ApplicationConfig,
  inject,
  provideAppInitializer,
  provideBrowserGlobalErrorListeners,
} from '@angular/core';
import { provideRouter, withComponentInputBinding } from '@angular/router';

import { provideZard } from '@/shared/core/provider/providezard';

import { routes } from './app.routes';
import { AuthService } from './core/auth.service';
import { ConfigService } from './core/config';

export const appConfig: ApplicationConfig = {
  providers: [
    provideBrowserGlobalErrorListeners(),
    provideRouter(routes, withComponentInputBinding()),
    provideZard(),
    provideAppInitializer(async () => {
      const config = inject(ConfigService);
      const auth = inject(AuthService);
      await config.load();
      await auth.init();
    }),
  ],
};
