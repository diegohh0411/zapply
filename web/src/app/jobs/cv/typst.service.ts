import { Injectable } from '@angular/core';

import type { CvContent } from './cv-content';
import type { CompileRequest, CompileResponse, CvTemplate } from './typst-protocol';

export type { CvTemplate } from './typst-protocol';

export interface Compiled {
  pages: number;
  pdf: Uint8Array;
}

/** Compiles CVs in a Web Worker (typst.worker.ts), started on first use. */
@Injectable({ providedIn: 'root' })
export class TypstService {
  private worker?: Worker;
  private seq = 0;
  private pending = new Map<
    number,
    { resolve: (c: Compiled) => void; reject: (e: Error) => void }
  >();

  compile(template: CvTemplate, content: CvContent): Promise<Compiled> {
    const worker = this.start();
    const id = ++this.seq;
    return new Promise<Compiled>((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      worker.postMessage({ id, template: template.assets, content } satisfies CompileRequest);
    });
  }

  private start(): Worker {
    if (this.worker) return this.worker;
    const worker = new Worker(new URL('./typst.worker', import.meta.url), { type: 'module' });
    worker.onmessage = ({ data }: MessageEvent<CompileResponse>) => {
      const p = this.pending.get(data.id);
      this.pending.delete(data.id);
      if (data.error || !data.pdf) p?.reject(new Error(data.error ?? 'The CV did not compile'));
      else p?.resolve({ pages: data.pages ?? 1, pdf: data.pdf });
    };
    worker.onerror = (e) => {
      for (const p of this.pending.values())
        p.reject(new Error(e.message || 'The CV compiler stopped'));
      this.pending.clear();
      worker.terminate();
      this.worker = undefined;
    };
    return (this.worker = worker);
  }
}
