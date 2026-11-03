// Messages between TypstService and typst.worker.ts. No Angular imports: the
// worker compiles with this file.
import type { components } from '../../api/schema';
import type { CvContent } from './cv-content';

export type CvTemplate = components['schemas']['CvTemplate'];

export interface CompileRequest {
  id: number;
  template: CvTemplate['assets'];
  content: CvContent;
}

export type WorkerRequest = CompileRequest | { type: 'preload' };

export interface CompileResponse {
  id: number;
  pages?: number;
  pdf?: Uint8Array;
  error?: string;
}
