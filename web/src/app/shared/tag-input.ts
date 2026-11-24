import {
  ChangeDetectionStrategy,
  Component,
  ElementRef,
  forwardRef,
  input,
  signal,
  viewChild,
} from '@angular/core';
import { type ControlValueAccessor, NG_VALUE_ACCESSOR } from '@angular/forms';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideX } from '@ng-icons/lucide';

import {
  comboboxChipsInputVariants,
  comboboxChipsVariants,
  comboboxChipVariants,
} from '@/shared/components/combobox/combobox.variants';

/** "  New   York " → "New York" */
const clean = (t: string): string => t.split(/\s+/).filter(Boolean).join(' ');

/**
 * A list of short terms typed as pills: Enter or a comma adds what was typed,
 * pasting "a, b" adds both, Backspace in an empty box removes the last, and
 * leaving the box keeps what was typed so a save never drops it. Duplicates
 * (ignoring case) are skipped. Binds a string[] with ngModel.
 */
@Component({
  selector: 'app-tag-input',
  imports: [NgIcon],
  changeDetection: ChangeDetectionStrategy.OnPush,
  viewProviders: [provideIcons({ lucideX })],
  providers: [{ provide: NG_VALUE_ACCESSOR, useExisting: forwardRef(() => TagInput), multi: true }],
  template: `
    <div [class]="box" (click)="focus()">
      @for (t of tags(); track t; let i = $index) {
        <span [class]="chip" data-slot="combobox-chip">
          {{ t }}
          <button
            type="button"
            class="-me-0.5 grid size-4 place-items-center rounded-sm opacity-60 hover:opacity-100 focus-visible:opacity-100 focus-visible:outline-2 focus-visible:outline-ring"
            [attr.aria-label]="'Remove ' + t"
            [disabled]="disabled()"
            (click)="remove(i); $event.stopPropagation()"
          >
            <ng-icon name="lucideX" class="size-3" aria-hidden="true" />
          </button>
        </span>
      }
      <input
        #box
        [class]="field"
        [attr.id]="inputId() || null"
        [attr.aria-label]="label() || null"
        [attr.aria-describedby]="describedBy() || null"
        [attr.aria-invalid]="invalid() || null"
        [placeholder]="tags().length ? '' : placeholder()"
        [disabled]="disabled()"
        [value]="text()"
        maxlength="100"
        autocomplete="off"
        enterkeyhint="enter"
        (input)="typed(box.value)"
        (keydown)="key($event)"
        (paste)="paste($event)"
        (blur)="commit(); touched()"
      />
    </div>
  `,
})
export class TagInput implements ControlValueAccessor {
  readonly placeholder = input('');
  readonly label = input('');
  readonly inputId = input('');
  readonly describedBy = input('');
  readonly invalid = input(false);
  /** The most terms the list may hold. */
  readonly max = input(20);

  protected readonly tags = signal<string[]>([]);
  protected readonly text = signal('');
  protected readonly disabled = signal(false);
  protected readonly box = comboboxChipsVariants() + ' min-h-8 cursor-text';
  protected readonly chip = comboboxChipVariants() + ' pe-1';
  protected readonly field =
    comboboxChipsInputVariants() + ' h-6 text-base placeholder:text-muted-foreground md:text-sm';
  private readonly boxRef = viewChild.required<ElementRef<HTMLInputElement>>('box');
  private onChange: (v: string[]) => void = () => {};
  protected touched: () => void = () => {};

  writeValue(v: string[] | null): void {
    this.tags.set(Array.isArray(v) ? v : []);
    this.text.set('');
  }
  registerOnChange(fn: (v: string[]) => void): void {
    this.onChange = fn;
  }
  registerOnTouched(fn: () => void): void {
    this.touched = fn;
  }
  setDisabledState(d: boolean): void {
    this.disabled.set(d);
  }

  protected focus(): void {
    this.boxRef().nativeElement.focus();
  }

  protected typed(value: string): void {
    if (value.includes(',')) {
      const parts = value.split(',');
      this.add(parts.slice(0, -1));
      this.text.set(parts.at(-1) ?? '');
      this.boxRef().nativeElement.value = this.text();
    } else {
      this.text.set(value);
    }
  }

  protected key(e: KeyboardEvent): void {
    if (e.key === 'Enter' && !e.isComposing) {
      // An empty box lets Enter submit the form as usual.
      if (clean(this.text())) {
        e.preventDefault();
        this.commit();
      }
    } else if (e.key === 'Backspace' && !this.text() && this.tags().length) {
      e.preventDefault();
      this.remove(this.tags().length - 1);
    }
  }

  protected paste(e: ClipboardEvent): void {
    const pasted = e.clipboardData?.getData('text') ?? '';
    if (!/[,\n]/.test(pasted)) return;
    e.preventDefault();
    this.add((this.text() + pasted).split(/[,\n]/));
    this.text.set('');
  }

  /** Adds what is typed so far. */
  commit(): void {
    if (!this.text()) return;
    this.add([this.text()]);
    this.text.set('');
  }

  protected remove(i: number): void {
    if (this.disabled()) return;
    this.tags.update((list) => list.filter((_, j) => j !== i));
    this.onChange(this.tags());
    this.focus();
  }

  private add(terms: string[]): void {
    const next = [...this.tags()];
    const seen = new Set(next.map((t) => t.toLowerCase()));
    for (const raw of terms) {
      const t = clean(raw).slice(0, 100);
      if (!t || seen.has(t.toLowerCase()) || next.length >= this.max()) continue;
      seen.add(t.toLowerCase());
      next.push(t);
    }
    if (next.length !== this.tags().length) {
      this.tags.set(next);
      this.onChange(next);
    }
  }
}
