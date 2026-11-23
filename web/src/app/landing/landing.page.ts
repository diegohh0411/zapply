import { ChangeDetectionStrategy, Component } from '@angular/core';
import { RouterLink } from '@angular/router';

import { ZardButtonComponent } from '@/shared/components/button';

const REPO_URL = 'https://github.com/diegohh0411/zapply';

/** Public home page: what zapply does and how to start. Signed-in users are sent to /jobs. */
@Component({
  selector: 'app-landing',
  imports: [RouterLink, ZardButtonComponent],
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="min-h-dvh bg-background text-foreground">
      <header class="mx-auto flex max-w-5xl items-center justify-between px-4 py-5 sm:px-6">
        <a routerLink="/" class="flex items-center gap-2 font-semibold tracking-tight">
          <img src="icon.svg" alt="" class="size-8 rounded-lg" />
          zapply
        </a>
        <a z-button zType="ghost" routerLink="/login">Sign in</a>
      </header>

      <main>
        <section class="mx-auto max-w-5xl px-4 pb-16 pt-12 sm:px-6 sm:pt-20">
          <p class="text-sm font-medium text-primary">Job applications, tailored</p>
          <h1 class="mt-3 max-w-3xl text-4xl font-semibold tracking-tight sm:text-5xl">
            Tailor your CV to each job, without starting from scratch.
          </h1>
          <p class="mt-5 max-w-2xl text-lg text-muted-foreground">
            zapply keeps your experience in one library, builds a CV for each posting from the
            evidence that fits it, and tracks every application from first send to reply.
          </p>
          <div class="mt-8 flex flex-wrap gap-3">
            <a z-button routerLink="/login">Get started</a>
            <a z-button zType="outline" [href]="repoUrl" target="_blank" rel="noopener">View the source</a>
          </div>
        </section>

        <section aria-labelledby="how-it-works" class="border-t bg-muted/40">
          <div class="mx-auto max-w-5xl px-4 py-16 sm:px-6">
            <h2 id="how-it-works" class="text-2xl font-semibold tracking-tight">How it works</h2>
            <ol class="mt-8 grid gap-6 sm:grid-cols-2">
              <li class="rounded-xl border bg-card p-6">
                <span class="text-sm font-medium text-primary">1</span>
                <h3 class="mt-2 text-lg font-semibold">Build your library</h3>
                <p class="mt-2 text-muted-foreground">
                  Record your experience, the organizations you worked with, and your skills once.
                  Every CV draws from this library.
                </p>
              </li>
              <li class="rounded-xl border bg-card p-6">
                <span class="text-sm font-medium text-primary">2</span>
                <h3 class="mt-2 text-lg font-semibold">Start from a job link</h3>
                <p class="mt-2 text-muted-foreground">
                  Paste the posting. zapply reads it and shows how well your library matches the
                  role, so you see the gaps before you apply.
                </p>
              </li>
              <li class="rounded-xl border bg-card p-6">
                <span class="text-sm font-medium text-primary">3</span>
                <h3 class="mt-2 text-lg font-semibold">Tailor and compile</h3>
                <p class="mt-2 text-muted-foreground">
                  Get a CV built from the evidence that fits this role. It is compiled in your
                  browser with Typst.
                </p>
              </li>
              <li class="rounded-xl border bg-card p-6">
                <span class="text-sm font-medium text-primary">4</span>
                <h3 class="mt-2 text-lg font-semibold">Track every application</h3>
                <p class="mt-2 text-muted-foreground">
                  Follow each job through its stages. Connect Gmail to see the mail for each
                  application alongside it.
                </p>
              </li>
            </ol>
          </div>
        </section>

        <section class="mx-auto max-w-5xl px-4 py-16 text-center sm:px-6">
          <h2 class="text-2xl font-semibold tracking-tight">Your next application starts with your library</h2>
          <p class="mx-auto mt-3 max-w-xl text-muted-foreground">
            Sign in to add your experience and paste your first job posting.
          </p>
          <div class="mt-8">
            <a z-button routerLink="/login">Sign in to zapply</a>
          </div>
        </section>
      </main>

      <footer class="border-t">
        <div class="mx-auto flex max-w-5xl flex-wrap items-center justify-between gap-3 px-4 py-6 text-sm text-muted-foreground sm:px-6">
          <span>zapply</span>
          <a class="underline-offset-4 hover:underline" [href]="repoUrl" target="_blank" rel="noopener">Source on GitHub</a>
        </div>
      </footer>
    </div>
  `,
})
export class LandingPage {
  readonly repoUrl = REPO_URL;
}
