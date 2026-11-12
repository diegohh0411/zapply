import {
  ChangeDetectionStrategy,
  Component,
  computed,
  DestroyRef,
  ElementRef,
  forwardRef,
  inject,
  input,
  signal,
  viewChild,
} from '@angular/core';
import { type ControlValueAccessor, NG_VALUE_ACCESSOR } from '@angular/forms';
import { NgIcon, provideIcons } from '@ng-icons/core';
import { lucideBold, lucideItalic } from '@ng-icons/lucide';

import { textareaVariants } from '@/shared/components/textarea';
import { mergeClasses } from '@/shared/utils/merge-classes';

import { parseRich, type Run, serializeRich } from './rich-text';

/** Shows marked text (see rich-text.ts) with its bold and italic. */
@Component({
  selector: 'app-rich-text',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `@for (r of runs(); track $index) {<span [class.font-semibold]="r.bold" [class.italic]="r.italic">{{ r.text }}</span>}`,
})
export class RichText {
  readonly text = input('');
  protected readonly runs = computed(() => parseRich(this.text() ?? ''));
}

/**
 * A one-paragraph editor for marked text. Cmd/Ctrl+B and Cmd/Ctrl+I toggle
 * bold and italic; pasted text arrives plain. The DOM is only ever built from
 * text nodes, <strong> and <em>.
 */
@Component({
  selector: 'app-rich-text-editor',
  imports: [NgIcon],
  providers: [
    { provide: NG_VALUE_ACCESSOR, useExisting: forwardRef(() => RichTextEditor), multi: true },
    provideIcons({ lucideBold, lucideItalic }),
  ],
  changeDetection: ChangeDetectionStrategy.OnPush,
  host: { class: 'block' },
  template: `
    <div [class]="frameClass()" (focusin)="refresh()">
      <div
        #ed
        class="min-h-11 flex-1 whitespace-pre-wrap break-words outline-none empty:before:text-muted-foreground empty:before:content-[attr(data-placeholder)]"
        contenteditable="true"
        role="textbox"
        aria-multiline="true"
        [attr.aria-label]="ariaLabel()"
        [attr.data-placeholder]="placeholder()"
        (input)="emit()"
        (blur)="onTouched()"
        (keydown)="key($event)"
        (keyup)="refresh()"
        (mouseup)="refresh()"
        (paste)="paste($event)"
        (drop)="$event.preventDefault()"
      ></div>
      <div class="-mr-1 -mt-1 flex shrink-0 gap-0.5 self-start">
        <button type="button" [class]="btnClass(active().bold)" title="Bold (⌘B)" aria-label="Bold" [attr.aria-pressed]="active().bold" (mousedown)="$event.preventDefault()" (click)="toggle('bold')">
          <ng-icon name="lucideBold" />
        </button>
        <button type="button" [class]="btnClass(active().italic)" title="Italic (⌘I)" aria-label="Italic" [attr.aria-pressed]="active().italic" (mousedown)="$event.preventDefault()" (click)="toggle('italic')">
          <ng-icon name="lucideItalic" />
        </button>
      </div>
    </div>
  `,
})
export class RichTextEditor implements ControlValueAccessor {
  readonly ariaLabel = input<string>('');
  readonly placeholder = input<string>('');
  readonly class = input<string>('');

  private readonly ed = viewChild.required<ElementRef<HTMLDivElement>>('ed');
  protected readonly active = signal({ bold: false, italic: false });
  private value = '';
  private onChange: (v: string) => void = () => {};
  protected onTouched: () => void = () => {};

  protected readonly frameClass = computed(() =>
    mergeClasses(textareaVariants(), 'gap-2 focus-within:border-ring focus-within:ring-3 focus-within:ring-ring/50 cursor-text', this.class()),
  );
  protected btnClass(on: boolean) {
    return mergeClasses(
      'inline-flex size-6 items-center justify-center rounded text-muted-foreground hover:bg-muted hover:text-foreground [&_ng-icon]:text-sm',
      on && 'bg-muted text-foreground',
    );
  }

  constructor() {
    const onSel = () => {
      const el = this.ed().nativeElement;
      if (el.contains(document.getSelection()?.anchorNode ?? null)) this.refresh();
    };
    document.addEventListener('selectionchange', onSel);
    inject(DestroyRef).onDestroy(() => document.removeEventListener('selectionchange', onSel));
  }

  writeValue(v: string | null): void {
    v ??= '';
    if (v === this.value && this.ed().nativeElement.childNodes.length) return;
    this.value = v;
    render(this.ed().nativeElement, parseRich(v));
  }
  registerOnChange(fn: (v: string) => void): void {
    this.onChange = fn;
  }
  registerOnTouched(fn: () => void): void {
    this.onTouched = fn;
  }
  setDisabledState(d: boolean): void {
    this.ed().nativeElement.contentEditable = d ? 'false' : 'true';
  }

  protected emit() {
    const el = this.ed().nativeElement;
    // An editor emptied by the user can keep a stray <br>; clear it so the placeholder shows.
    if (!el.textContent) el.replaceChildren();
    this.value = serializeRich(readRuns(el));
    this.onChange(this.value);
    this.refresh();
  }

  protected refresh() {
    this.active.set({ bold: document.queryCommandState('bold'), italic: document.queryCommandState('italic') });
  }

  protected toggle(cmd: 'bold' | 'italic') {
    this.ed().nativeElement.focus();
    document.execCommand('styleWithCSS', false, 'false');
    document.execCommand(cmd);
    this.emit();
  }

  protected key(e: KeyboardEvent) {
    if (e.key === 'Enter') return e.preventDefault();
    if (!(e.metaKey || e.ctrlKey) || e.altKey) return;
    const k = e.key.toLowerCase();
    if (k === 'b' || k === 'i') {
      e.preventDefault();
      this.toggle(k === 'b' ? 'bold' : 'italic');
    } else if (k === 'u') e.preventDefault();
  }

  protected paste(e: ClipboardEvent) {
    e.preventDefault();
    const text = (e.clipboardData?.getData('text/plain') ?? '').replace(/\s*\n\s*/g, ' ');
    document.execCommand('insertText', false, text);
  }
}

function render(el: HTMLElement, runs: Run[]) {
  el.replaceChildren(
    ...runs.map((r) => {
      let n: Node = document.createTextNode(r.text);
      if (r.italic) {
        const em = document.createElement('em');
        em.append(n);
        n = em;
      }
      if (r.bold) {
        const s = document.createElement('strong');
        s.append(n);
        n = s;
      }
      return n;
    }),
  );
}

function readRuns(root: HTMLElement): Run[] {
  const runs: Run[] = [];
  const walk = (node: Node, bold: boolean, italic: boolean) => {
    if (node.nodeType === Node.TEXT_NODE) {
      runs.push({ text: (node.textContent ?? '').replace(/ /g, ' ').replace(/\n/g, ' '), bold, italic });
      return;
    }
    if (!(node instanceof HTMLElement)) return;
    if (node.tagName === 'BR') {
      if (node !== root.lastChild) runs.push({ text: ' ', bold, italic });
      return;
    }
    const tag = node.tagName;
    const st = node.style;
    const w = st.fontWeight;
    if (tag === 'B' || tag === 'STRONG' || w === 'bold' || Number(w) >= 600) bold = true;
    if (w === 'normal' || (w && Number(w) < 600)) bold = false;
    if (tag === 'I' || tag === 'EM' || st.fontStyle === 'italic') italic = true;
    if (st.fontStyle === 'normal') italic = false;
    const block = tag === 'DIV' || tag === 'P';
    if (block && runs.length) runs.push({ text: ' ', bold: false, italic: false });
    node.childNodes.forEach((c) => walk(c, bold, italic));
  };
  root.childNodes.forEach((c) => walk(c, false, false));
  return runs;
}
