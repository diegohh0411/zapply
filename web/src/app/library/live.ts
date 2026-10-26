import { DestroyRef, inject } from '@angular/core';
import { takeUntilDestroyed } from '@angular/core/rxjs-interop';
import { filter, merge } from 'rxjs';

import { EventsService } from '../core/events.service';

/**
 * Calls reload whenever an event whose topic starts with one of the given
 * prefixes arrives, and after a resync. Call from an injection context.
 */
export function onChanges(prefixes: string[], reload: () => void): void {
  const events = inject(EventsService);
  merge(events.resync, events.changes.pipe(filter((e) => prefixes.some((p) => e.topic.startsWith(p)))))
    .pipe(takeUntilDestroyed(inject(DestroyRef)))
    .subscribe(() => reload());
}

/** Field errors of an ApiError as a map, for templates. */
export function fieldMap(e: unknown): Record<string, string> {
  const errs = (e as { fieldErrors?: { field: string; message: string }[] }).fieldErrors ?? [];
  return Object.fromEntries(errs.map((f) => [f.field, f.message]));
}
