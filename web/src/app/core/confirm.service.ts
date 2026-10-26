import { effect, inject, Injectable, Injector } from '@angular/core';

import { ZardAlertDialogService } from '@/shared/components/alert-dialog';

export interface ConfirmOptions {
  title: string;
  description?: string;
  /** Label of the confirm button. Default "Delete". */
  okText?: string;
  /** Style the confirm button as destructive. Default true. */
  destructive?: boolean;
}

/**
 * Promise-based confirmation on ZardUI's alert dialog. Resolves true only
 * when the user confirms; Cancel, Escape and the close button resolve false.
 */
@Injectable({ providedIn: 'root' })
export class ConfirmService {
  private readonly dialogs = inject(ZardAlertDialogService);
  private readonly injector = inject(Injector);

  ask(o: ConfirmOptions): Promise<boolean> {
    return new Promise((resolve) => {
      let confirmed = false;
      const ref = this.dialogs.confirm({
        zTitle: o.title,
        zDescription: o.description,
        zOkText: o.okText ?? 'Delete',
        zOkDestructive: o.destructive ?? true,
        zOnOk: () => {
          confirmed = true;
        },
      });
      const watch = effect(
        () => {
          if (ref.isClosing()) {
            resolve(confirmed);
            watch.destroy();
          }
        },
        { injector: this.injector },
      );
    });
  }
}
