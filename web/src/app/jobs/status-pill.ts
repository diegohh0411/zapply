import { ChangeDetectionStrategy, Component, input } from '@angular/core';
import type { JobStatus } from './jobs.store';

@Component({
  selector: 'app-status-pill',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `<span class="status-pill" [class]="status().color"><span class="status-dot" aria-hidden="true"></span>{{ status().name }}</span>`,
  styles: `
    :host { display: inline-flex; min-width: 0; }
    .status-pill { display: inline-flex; align-items: center; gap: 6px; max-width: 100%; border-radius: 9999px; padding: 2px 8px; font-size: 12px; font-weight: 500; line-height: 18px; white-space: nowrap; background: var(--status-bg); color: var(--status-fg); }
    .status-dot { width: 7px; height: 7px; flex: none; border-radius: 50%; background: currentColor; }
    .gray { --status-bg: #e9e9e7; --status-fg: #787774; }
    .brown { --status-bg: #eee0da; --status-fg: #9f6b53; }
    .orange { --status-bg: #fadec9; --status-fg: #d9730d; }
    .yellow { --status-bg: #fdecc8; --status-fg: #cb912f; }
    .green { --status-bg: #dbeddb; --status-fg: #448361; }
    .blue { --status-bg: #d3e5ef; --status-fg: #337ea9; }
    .purple { --status-bg: #e8deee; --status-fg: #9065b0; }
    .pink { --status-bg: #f5e0e9; --status-fg: #c14c8a; }
    .red { --status-bg: #ffe2dd; --status-fg: #d44c47; }
    :host-context(.dark) .status-pill { background: color-mix(in srgb, var(--status-fg) 25%, #202020); color: color-mix(in srgb, var(--status-fg) 65%, white); }
  `,
})
export class StatusPill {
  readonly status = input.required<Pick<JobStatus, 'name' | 'color'>>();
}
