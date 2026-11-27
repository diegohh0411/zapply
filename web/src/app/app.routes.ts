import { inject } from '@angular/core';
import { type CanActivateFn, Router, type Routes } from '@angular/router';

import { AuthService } from './core/auth.service';
import { AccountService } from './core/account.service';

const signedIn: CanActivateFn = () =>
  inject(AuthService).signedIn() || inject(Router).parseUrl('/login');
const signedOut: CanActivateFn = () =>
  !inject(AuthService).signedIn() || inject(Router).parseUrl('/jobs');
const owner: CanActivateFn = async () => {
  const account = inject(AccountService),
    router = inject(Router);
  const me = await account.refresh();
  return me?.platform_role === 'owner' || router.parseUrl('/jobs');
};

export const routes: Routes = [
  {
    // Public home page. Signed-in users are sent straight to the app.
    path: '',
    pathMatch: 'full',
    title: 'zapply · Tailor your CV to each job',
    canActivate: [signedOut],
    loadComponent: () => import('./landing/landing.page').then((m) => m.LandingPage),
  },
  {
    path: 'login',
    title: 'Sign in · zapply',
    canActivate: [signedOut],
    data: {
      description: 'Sign in to zapply to tailor your CV to each job and track every application.',
    },
    loadComponent: () => import('./auth/login.page').then((m) => m.LoginPage),
  },
  {
    path: '',
    canActivate: [signedIn],
    // Signed-in pages are private to the user: keep them out of search results.
    data: { robots: 'noindex, nofollow' },
    loadComponent: () => import('./layout/shell').then((m) => m.Shell),
    children: [
      { path: '', pathMatch: 'full', redirectTo: 'jobs' },
      {
        path: 'jobs',
        title: 'Jobs · zapply',
        loadComponent: () => import('./jobs/jobs.page').then((m) => m.JobsPage),
      },
      {
        path: 'notifications',
        title: 'Notifications · zapply',
        loadComponent: () =>
          import('./notifications/notifications.page').then((m) => m.NotificationsPage),
      },
      // Mail was split: its updates are notifications, its mailboxes are connections.
      { path: 'mail', redirectTo: 'notifications' },
      {
        path: 'usage',
        title: 'Usage · zapply',
        loadComponent: () => import('./usage/usage.page').then((m) => m.UsagePage),
      },
      {
        path: 'settings',
        loadComponent: () => import('./settings/settings.layout').then((m) => m.SettingsLayout),
        children: [
          { path: '', pathMatch: 'full', redirectTo: 'account' },
          {
            path: 'account',
            title: 'Account settings · zapply',
            loadComponent: () =>
              import('./settings/account.page').then((m) => m.AccountSettingsPage),
          },
          {
            path: 'connections',
            title: 'Connections · zapply',
            loadComponent: () =>
              import('./settings/connections.page').then((m) => m.ConnectionsPage),
          },
          {
            path: 'job-preferences',
            title: 'Job preferences · zapply',
            loadComponent: () =>
              import('./settings/job-preferences.page').then((m) => m.JobPreferencesPage),
          },
        ],
      },
      { path: 'admin', pathMatch: 'full', redirectTo: 'admin/overview' },
      // Renamed: the keys page now owns the key list and the access check.
      { path: 'admin/system-keys', redirectTo: 'admin/keys' },
      {
        path: 'admin/:section',
        canActivate: [owner],
        title: 'Platform administration · zapply',
        loadComponent: () => import('./admin/admin.page').then((m) => m.AdminPage),
      },
      {
        path: 'library',
        children: [
          { path: '', pathMatch: 'full', redirectTo: 'experience' },
          {
            path: 'profile',
            title: 'Profile · zapply',
            loadComponent: () => import('./library/profile.page').then((m) => m.ProfilePage),
          },
          {
            path: 'experience',
            title: 'Experience · zapply',
            loadComponent: () => import('./library/evidence.page').then((m) => m.EvidencePage),
          },
          {
            path: 'organizations',
            title: 'Organizations · zapply',
            loadComponent: () =>
              import('./library/organizations.page').then((m) => m.OrganizationsPage),
          },
          {
            path: 'skills',
            title: 'Skills · zapply',
            loadComponent: () => import('./library/skills.page').then((m) => m.SkillsPage),
          },
        ],
      },
    ],
  },
  { path: '**', redirectTo: 'jobs' },
];
