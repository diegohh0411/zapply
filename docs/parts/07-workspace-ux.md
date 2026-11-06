# Match and CV workspace

The primary task is one continuous flow: review the match, adjust selections,
export a PDF. The previous separate Match and CV tabs split that task and
repeated the selection list. The combined **Match & CV** tab keeps existing
`tab=match` and `tab=cv` links working.

## Design direction

Use CVD's selection workspace as the reference: experience checkboxes, nested
highlight checkboxes, compact relevance bars, and an adjacent PDF. The local
reference is `cvd/apps/web/src/app/features/workspace/selection-list.component.ts`
from the existing CVD checkout. Avoid its drag affordances because this app
currently sorts the document chronologically and has no saved manual ordering.

Retain zapply's white (`#ffffff`) and charcoal (`#171717`) foundation, neutral
secondary text (`#737373`), dividers (`#e5e5e5`), and existing fuchsia brand token.
Keep the app's sans-serif typography: a 20px workspace title, 15px section titles,
14px experience titles, and 13px highlight text. Selection is left aligned;
relevance and selection policy sit beside the item they describe. Use restrained
8px experience-card corners and no additional shadows.

Desktop layout:

```text
Match & CV                                  Export PDF
Match results / processing status / re-run
Choose your experience       PDF preview / language / length
[✓] Experience    [score]     ┌───────────────────────────┐
    Automatic / Always keep  │                           │
    [✓] Highlight            │        Fitted CV          │
    [ ] Highlight            │                           │
[ ] Other experience         └───────────────────────────┘
Edit professional summary
Export history
```

The PDF stays alongside the editor from 1024px upwards. Smaller screens stack
the editor and preview; the export toolbar remains sticky above phone sizes.
No guessed overall match score or page-fill percentage is shown: relevance is
per experience and page counts come from the actual compiler.

## Interaction and content rules

- Results appear first. A compact overview avoids repeating the editable list.
- Checkboxes replace eye and pin icons. Checking an omitted item always keeps
  it; unchecking leaves it out. A labeled policy select can return an entry to
  automatic selection. Highlight overrides have a “Use automatic” action.
- Included highlights are expanded. Omitted entries keep their details folded.
- Summary editing, technical match details, and export history are disclosed
  when needed rather than competing with the main selection task.
- Selection changes save automatically and update the existing PDF in place.
  Export waits for a current fit. Unsaved summary edits require saving first.
- Re-running matching explicitly confirms that manual selections will reset.
- Eligible library experience fills pages even below the match threshold.
  Library exclusions and career-break opt-in remain respected. Optional
  content trims by priority, then smaller items refill spare space. Exports
  record the exact omissions after backfilling, rather than a trim prefix.

## UX sources

[Nielsen Norman Group: 10 usability heuristics](https://www.nngroup.com/articles/ten-usability-heuristics/)
supports visible system status, recognition over recall, user control, and
keeping secondary information from obscuring the task. Here those translate
into current-preview feedback, labeled selection policies, reversible choices,
and the combined editing flow.

[W3C WAI: labeling controls](https://www.w3.org/WAI/tutorials/forms/labels/)
supports native checkboxes with associated text labels and named select fields.
All selection controls work with the keyboard and retain visible focus states.
