# Add a job: capture first, enrich later

The old creation page gave every property equal visual weight. A title-only save still looked like a long application form, with a one-item tab bar and an action below the fold.

## What Notion does

- **Multiple entry points:** a database-level New button and contextual New items in tables, lists, boards, galleries, and calendars. Calendar creation carries the selected date. Each item is a page, not just a row. [Database pages](https://www.notion.com/help/intro-to-databases)
- **Peek before full page:** views can open records in side peek, center peek, or full page. Side peek keeps the database interactive beside the record; center peek provides a focused modal. Table, board, list, and timeline views default to side peek. [View settings](https://www.notion.com/help/views-filters-and-sorts)
- **Properties followed by content:** the page presents property names and editable values, with a freeform content area underneath. This gives structured metadata and longer content different roles. [Database pages](https://www.notion.com/help/intro-to-databases)
- **Templates:** the New dropdown can create an item with preset properties and page content. A template can also be selected inside a new page. [Database templates](https://www.notion.com/help/database-templates)
- **Configurable information density:** database page layouts can organize properties into groups or a details panel. [Layouts](https://www.notion.com/help/layouts)

These are documented product behaviors, checked November 5, 2026. The choices below are adaptations for zapply.

## Design direction

Retain the existing application font and semantic theme tokens, including dark mode. Reference light palette: white #FFFFFF, charcoal #171717, muted gray #737373, border #E5E5E5, and zapply purple approximately #C026D3. Use a 30px title, 14px property text, and 12px supporting text. Keep a left-aligned editor, capped at 680px, with a persistent header and save bar.

The distinctive element is the job title itself. Avoid additional cards, a wizard, or a template chooser: this is a tool for collecting opportunities while browsing jobs, and a title-only save should feel lightweight.

## Implemented flow

1. Choose New job from the database, empty state, table, or board column.
2. Open a center dialog above the current database. Focus the job title. A column's status carries into the draft; otherwise prefer the saved stage.
3. Enter the title. Company, posting link, and status are immediately available in compact property rows.
4. Optionally expand Job description, More properties, or Notes. More properties contains location, work mode, employment, salary, source, and applied date. All existing creation fields remain available.
5. Choose Save job, or use Cmd/Ctrl+Enter. The title is required; an empty/whitespace title cannot be saved. Browser validation checks posting URL and numeric constraints. Repeated submission is blocked while saving.
6. On success, open the saved job workspace with Details, Match, CV, Mail, and Activity. The existing duplicate-posting warning can open the previously saved job. Server errors keep the draft, expose the relevant optional section, and display the error.
7. Close or Escape on an edited draft asks whether to discard it. Cancelling preserves the input. Backdrop clicks do not dismiss capture. Focus trapping and restoration use the application's dialog component.

This keeps creation to one screen. Users can save immediately or enrich a record in the same place. Salary and application dates no longer compete with the title.

NN/g recommends grouping related fields, reducing optional questions, and progressively revealing detail to reduce the effort users perceive in a form. This supports the capture-first structure. [Form design research](https://www.nngroup.com/articles/4-principles-reduce-cognitive-load/)

## Extraction first

Pasting a posting link is now the first step: the read starts on paste, a loader names what is happening (opening the page, picking out the details), and the same dialog opens on a filled form whose extracted fields are marked. "Fill it in myself" opens the form early without losing the answer, Cancel returns to the link, and a failed read still opens the form with a note. Posting text can be pasted under a disclosure when a page needs a login.

## Further opportunities

- Inline title-only row creation for people entering many jobs in succession.
- A side peek for existing jobs when comparing several opportunities; the current saved-job workspace remains full page.
- Templates only after repeated setups emerge, for example a referral checklist or interview preparation notes.

## Validation

Production Angular build checks templates. Frontend regression tests cover title-only creation, status defaults and column overrides, duplicate submission, cancelled discard, Escape restoration, and closing during an in-flight save. Browser interaction and visual checks remain manual.
