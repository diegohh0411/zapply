import { Injectable, signal } from '@angular/core';

/** Light/dark toggle, persisted the way index.html's boot script reads it. */
@Injectable({ providedIn: 'root' })
export class ThemeService {
  readonly dark = signal(document.documentElement.classList.contains('dark'));

  toggle(): void {
    const dark = !this.dark();
    this.dark.set(dark);
    localStorage.setItem('theme', dark ? 'dark' : 'light');
    const html = document.documentElement;
    html.classList.toggle('dark', dark);
    html.classList.toggle('dark-theme', dark);
    html.setAttribute('data-theme', dark ? 'dark' : 'light');
    html.style.colorScheme = dark ? 'dark' : 'light';
  }
}
